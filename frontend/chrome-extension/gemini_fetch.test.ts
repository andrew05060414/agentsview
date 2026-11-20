import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { describe, expect, it, vi } from "vite-plus/test";

const source =
  readFileSync("chrome-extension/claude_fetch.js", "utf8") +
  "\n" +
  readFileSync("chrome-extension/gemini_fetch.js", "utf8");

const gaiaID = "123456789012345678901";
const pageData = `<script>window.WIZ_global_data = {"SNlM0e":"synthetic-token","FdrFJe":"sid-1","cfb2h":"build-1","obfuscatedGaiaId":"${gaiaID}","oPEP7c":"someone@example.com"};</script>`;

function loadFetch(fetch: typeof globalThis.fetch, { pathname = "/app" } = {}) {
  const context: Record<string, unknown> = {
    AbortController,
    TextDecoder,
    TextEncoder,
    URL,
    URLSearchParams,
    clearTimeout,
    crypto,
    fetch,
    setTimeout,
    location: { origin: "https://gemini.google.com", pathname },
    document: { documentElement: { innerHTML: pageData } },
  };
  runInNewContext(source, context);
  return context.geminiFetch as (url: string) => Promise<{ status: number; body: string }>;
}

const base = "https://gemini.google.com/u/0/_/BardChatUi/data/batchexecute?rpcids=";

function sent(fetch: ReturnType<typeof vi.fn>, call: number) {
  const [url, init] = fetch.mock.calls[call]! as [string, RequestInit];
  const form = new URLSearchParams(String(init.body));
  const request = JSON.parse(form.get("f.req")!);
  return {
    url: new URL(url),
    init,
    at: form.get("at"),
    rpc: request[0][0][0],
    args: JSON.parse(request[0][0][1]),
  };
}

describe("Gemini page fetch", () => {
  it("posts the list RPC with the slot's page token and returns the raw response", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(new Response(pageData))
      .mockResolvedValueOnce(new Response(")]}'\n[]"));
    const geminiFetch = loadFetch(fetch);

    const response = await geminiFetch(`${base}MaZiqc&cursor=`);

    expect(response).toEqual({ status: 200, body: ")]}'\n[]", retryAfter: undefined });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(fetch.mock.calls[0]![0]).toBe("https://gemini.google.com/app");
    const call = sent(fetch, 1);
    expect(call.url.pathname).toBe("/_/BardChatUi/data/batchexecute");
    expect(call.url.searchParams.get("rpcids")).toBe("MaZiqc");
    expect(call.url.searchParams.get("bl")).toBe("build-1");
    expect(call.url.searchParams.get("f.sid")).toBe("sid-1");
    expect(call.init).toMatchObject({
      method: "POST",
      credentials: "include",
      headers: { "X-Same-Domain": "1" },
    });
    expect(call.at).toBe("synthetic-token");
    expect([call.rpc, call.args]).toEqual(["MaZiqc", [20]]);
    expect(JSON.stringify(response)).not.toContain("synthetic-token");
  });

  it("reads each account slot with its own token, whatever account the tab shows", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(new Response(pageData.replace("synthetic-token", "slot-1-token")))
      .mockResolvedValueOnce(new Response("ok"))
      .mockResolvedValueOnce(new Response("ok"));
    const geminiFetch = loadFetch(fetch, { pathname: "/u/3/app/0123456789abcdef" });

    await geminiFetch(
      `${base.replace("/u/0/", "/u/1/")}hNvQHb&chat=c_0123456789abcdef&cursor=ab%2Bcd%2F%3D`,
    );
    await geminiFetch(`${base.replace("/u/0/", "/u/1/")}MaZiqc&cursor=`);

    expect(fetch.mock.calls[0]![0]).toBe("https://gemini.google.com/u/1/app");
    const call = sent(fetch, 1);
    expect(call.url.pathname).toBe("/u/1/_/BardChatUi/data/batchexecute");
    expect(call.at).toBe("slot-1-token");
    expect(call.args).toEqual(["c_0123456789abcdef", 20, "ab+cd/=", 1, [0], [4], null, 1]);
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(sent(fetch, 2).at).toBe("slot-1-token");
  });

  it("reports sign-in when the slot's app page has no token", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValue(new Response("<html>Sign in</html>"));
    const geminiFetch = loadFetch(fetch);

    expect(await geminiFetch(`${base}MaZiqc&cursor=`)).toEqual({ status: 401, body: "" });
    expect(fetch).toHaveBeenCalledOnce();
    expect(fetch.mock.calls[0]![1]).toMatchObject({ method: "GET" });
  });

  it("fetches a fresh token once after a refusal", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(new Response(pageData))
      .mockResolvedValueOnce(new Response("", { status: 400 }))
      .mockResolvedValueOnce(new Response(pageData.replace("synthetic-token", "fresh-token")))
      .mockResolvedValueOnce(new Response("ok"));
    const geminiFetch = loadFetch(fetch);

    expect(await geminiFetch(`${base}MaZiqc&cursor=`)).toMatchObject({ status: 200, body: "ok" });
    expect(sent(fetch, 3).at).toBe("fresh-token");
  });

  it("answers the identity request with a hash of the account ID only", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>().mockResolvedValue(new Response(pageData));
    const geminiFetch = loadFetch(fetch);

    const response = await geminiFetch("https://gemini.google.com/u/2/app");

    expect(fetch.mock.calls[0]![0]).toBe("https://gemini.google.com/u/2/app");
    const account = createHash("sha256").update(gaiaID).digest("hex").slice(0, 12);
    expect(response).toEqual({ status: 200, body: JSON.stringify({ account }) });
    expect(response.body).not.toContain(gaiaID);
    expect(response.body).not.toContain("example.com");
    expect(response.body).not.toContain("synthetic-token");
  });

  it("answers an empty identity when the page names no account", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValue(new Response(pageData.replace(gaiaID, "not-an-id")));
    const geminiFetch = loadFetch(fetch);

    expect(await geminiFetch("https://gemini.google.com/u/0/app")).toEqual({
      status: 200,
      body: JSON.stringify({ account: "" }),
    });
  });

  it("refuses requests without an account slot and RPCs outside the two read calls", async () => {
    const fetch = vi.fn<typeof globalThis.fetch>();
    const geminiFetch = loadFetch(fetch);

    expect(await geminiFetch(`${base}ESY5D&cursor=`)).toMatchObject({ status: 0 });
    expect(await geminiFetch(`${base.replace("/u/0", "")}MaZiqc&cursor=`)).toMatchObject({
      status: 0,
    });
    expect(fetch).not.toHaveBeenCalled();
  });
});
