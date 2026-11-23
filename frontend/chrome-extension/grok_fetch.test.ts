import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { describe, expect, it, vi } from "vite-plus/test";

const source =
  readFileSync("chrome-extension/claude_fetch.js", "utf8") +
  "\n" +
  readFileSync("chrome-extension/grok_fetch.js", "utf8");

function loadFetch(fetch: typeof globalThis.fetch) {
  const context: Record<string, unknown> = {
    AbortController,
    TextDecoder,
    TextEncoder,
    URL,
    clearTimeout,
    crypto,
    fetch,
    setTimeout,
  };
  runInNewContext(`${source}\nglobalThis.grokFetch = grokFetch;`, context);
  return context.grokFetch as (url: string) => Promise<unknown>;
}

describe("Grok page fetch", () => {
  it("sends a cookie GET that asks for JSON", async () => {
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(new Response('{"conversations":[]}'));
    const grokFetch = loadFetch(fetch);
    const url = "https://grok.com/rest/app-chat/conversations?pageSize=100";

    expect(await grokFetch(url)).toEqual({
      status: 200,
      body: '{"conversations":[]}',
      retryAfter: undefined,
    });
    expect(fetch).toHaveBeenCalledExactlyOnceWith(
      url,
      expect.objectContaining({
        method: "GET",
        credentials: "include",
        redirect: "error",
        headers: { Accept: "application/json" },
      }),
    );
  });

  it("answers the identity request with a hash of the user ID only", async () => {
    const userId = "55555555-5555-4555-8555-555555555555";
    const fetch = vi
      .fn<typeof globalThis.fetch>()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ userId, email: "someone@example.com" })),
      );
    const grokFetch = loadFetch(fetch);

    const account = createHash("sha256").update(userId).digest("hex").slice(0, 12);
    expect(await grokFetch("https://grok.com/rest/subscriptions")).toEqual({
      status: 200,
      body: JSON.stringify({ account }),
    });
  });
});
