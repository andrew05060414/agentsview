import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { describe, expect, it, vi } from "vite-plus/test";

function adapter(platform: "gemini" | "grok", fetch: ReturnType<typeof vi.fn>) {
  const context = { URL, URLSearchParams, TextEncoder, claudeFetch: fetch };
  runInNewContext(readFileSync(`chrome-extension/${platform}_fetch.js`, "utf8"), context);
  return (
    context as unknown as Record<string, (url: string) => Promise<{ status: number; body: string }>>
  )[`${platform}Fetch`]!;
}

describe("Gemini read RPC adapter", () => {
  it("keeps session fields in the page and permits only fixed read RPCs", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce({
        status: 200,
        body: '{"SNlM0e":"private-session","cfb2h":"build","FdrFJe":"123"}',
      })
      .mockResolvedValueOnce({ status: 200, body: "history" });
    const result = await adapter(
      "gemini",
      fetch,
    )("https://gemini.google.com/agentsview-sync/conversations?pinned=1&cursor=next%2Bpage");
    expect(result).toEqual({ status: 200, body: "history" });
    const call = fetch.mock.calls[1]!;
    expect(new URL(call[0]).searchParams.get("rpcids")).toBe("MaZiqc");
    const form = new URLSearchParams(call[2].body);
    expect(form.get("at")).toBe("private-session");
    const envelope = JSON.parse(form.get("f.req")!);
    expect(JSON.parse(envelope[0][0][1])).toEqual([50, "next+page", [1, null, 1]]);
    expect(JSON.stringify(result)).not.toContain("private-session");
    await expect(
      adapter(
        "gemini",
        fetch,
      )("https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=GzXR5e"),
    ).rejects.toThrow("Unsupported Gemini read");
    expect(fetch).toHaveBeenCalledTimes(2);
  });
  it("asks for sign-in without relaying the app page when the token is absent", async () => {
    const fetch = vi.fn().mockResolvedValue({ status: 200, body: "login page" });
    expect(
      await adapter(
        "gemini",
        fetch,
      )("https://gemini.google.com/agentsview-sync/conversations?pinned=0&cursor="),
    ).toEqual({ status: 401, body: "" });
    expect(fetch).toHaveBeenCalledOnce();
  });
  it("refreshes session fields once after an expired read token", async () => {
    const page = { status: 200, body: '{"SNlM0e":"session"}' };
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(page)
      .mockResolvedValueOnce({ status: 401, body: "" })
      .mockResolvedValueOnce(page)
      .mockResolvedValueOnce({ status: 200, body: "history" });
    expect(
      await adapter(
        "gemini",
        fetch,
      )("https://gemini.google.com/agentsview-sync/conversation/c_12345678?limit=100"),
    ).toEqual({ status: 200, body: "history" });
    expect(fetch).toHaveBeenCalledTimes(4);
  });
});

describe("Grok read adapter", () => {
  const id = "33333333-3333-4333-8333-333333333333";
  it("loads response bodies through a fixed read endpoint", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce({
        status: 200,
        body: JSON.stringify({ responseNodes: [{ responseId: id }], inflightResponses: [] }),
      })
      .mockResolvedValueOnce({
        status: 200,
        body: JSON.stringify({ responses: [{ responseId: id, message: "Answer" }] }),
      });
    const result = await adapter(
      "grok",
      fetch,
    )(`https://grok.com/rest/app-chat/conversations/${id}/response-node?includeThreads=true`);
    expect(JSON.parse(result.body).responses).toHaveLength(1);
    expect(fetch.mock.calls[1]![0]).toBe(
      `https://grok.com/rest/app-chat/conversations/${id}/load-responses`,
    );
    expect(JSON.parse(fetch.mock.calls[1]![2].body)).toEqual({ responseIds: [id] });
  });
  it("propagates failed body reads instead of returning a partial chat", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce({
        status: 200,
        body: JSON.stringify({ responseNodes: [{ responseId: id }] }),
      })
      .mockResolvedValueOnce({ status: 429, body: "", retryAfter: "120" });
    expect(
      await adapter(
        "grok",
        fetch,
      )(`https://grok.com/rest/app-chat/conversations/${id}/response-node?includeThreads=true`),
    ).toMatchObject({ status: 429, retryAfter: "120" });
  });
});
