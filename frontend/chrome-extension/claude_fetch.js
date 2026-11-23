// worker.js injects this file, then calls claudeFetch from a second script.
// A request body turns the request into a POST; only gemini_fetch.js sends one.
globalThis.claudeFetch = async function claudeFetch(url, headers, requestBody) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 45000);
  try {
    const response = await fetch(url, {
      method: requestBody === undefined ? "GET" : "POST",
      body: requestBody,
      credentials: "include",
      redirect: "error",
      headers,
      signal: controller.signal,
    });
    const limit = 32 * 1024 * 1024; // Browser sync caps each response at 32 MiB.
    const reader = response.body?.getReader();
    const decoder = new TextDecoder();
    let size = 0;
    let body = "";
    if (reader) {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        size += value.byteLength;
        if (size > limit) {
          controller.abort();
          try {
            await reader.cancel();
          } catch {}
          return { status: 413, body: "" };
        }
        body += decoder.decode(value, { stream: true });
      }
      body += decoder.decode();
    }
    if (new URL(url).pathname === "/api/organizations") {
      if (!response.ok)
        return {
          status: response.status,
          body: "",
          retryAfter: response.headers.get("retry-after") ?? undefined,
        };
      try {
        const organizations = JSON.parse(body);
        body = JSON.stringify(
          organizations.map(({ uuid, capabilities }) => ({ uuid, capabilities })),
        );
      } catch {
        return { status: 0, body: "", error: "Unexpected organization response" };
      }
    }
    const retryAfter = response.headers.get("retry-after") ?? undefined;
    return { status: response.status, body, retryAfter };
  } catch (error) {
    return { status: 0, body: "", error: String(error) };
  } finally {
    clearTimeout(timer);
  }
};

globalThis.browserAccountReply = async function browserAccountReply(identity) {
  let account = "";
  if (typeof identity === "string" && identity) {
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(identity));
    account = Array.from(new Uint8Array(digest, 0, 6), (byte) =>
      byte.toString(16).padStart(2, "0"),
    ).join("");
  }
  return { status: 200, body: JSON.stringify({ account }) };
};
