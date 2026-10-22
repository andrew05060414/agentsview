// Virtual read paths select fixed RPCs. Session values never cross the relay.
globalThis.geminiFetch = async function geminiFetch(url) {
  const request = new URL(url);
  if (request.origin !== "https://gemini.google.com") throw new Error("Unsupported Gemini origin");
  const list = request.pathname === "/agentsview-sync/conversations";
  const detail = /^\/agentsview-sync\/conversation\/(c_[a-f0-9]{8,64})$/.exec(request.pathname);
  if (!list && !detail) throw new Error("Unsupported Gemini read");
  const rpc = list ? "MaZiqc" : "hNvQHb";
  const payload = list
    ? [
        50,
        request.searchParams.get("cursor") || null,
        [Number(request.searchParams.get("pinned")), null, 1],
      ]
    : [detail[1], Number(request.searchParams.get("limit")), null, 1, [1], [4], null, 1];
  for (let attempt = 0; attempt < 2; attempt++) {
    const page = await globalThis.claudeFetch("https://gemini.google.com/app");
    if (page.status !== 200) return { status: page.status, body: "", retryAfter: page.retryAfter };
    const field = (name) => {
      const match = new RegExp(`"${name}":("(?:[^"\\\\]|\\\\.)*")`).exec(page.body);
      return match ? JSON.parse(match[1]) : undefined;
    };
    const token = field("SNlM0e");
    if (!token) return { status: 401, body: "" };
    const query = new URLSearchParams({ rpcids: rpc, "source-path": "/app", hl: "en", rt: "c" });
    const build = field("cfb2h");
    const session = field("FdrFJe");
    if (build) query.set("bl", build);
    if (session) query.set("f.sid", session);
    const body = new URLSearchParams({
      "f.req": JSON.stringify([[[rpc, JSON.stringify(payload), null, "generic"]]]),
      at: token,
    }).toString();
    const response = await globalThis.claudeFetch(
      `https://gemini.google.com/_/BardChatUi/data/batchexecute?${query}`,
      { "content-type": "application/x-www-form-urlencoded;charset=UTF-8", "x-same-domain": "1" },
      { method: "POST", body },
    );
    if (attempt === 0 && [400, 401].includes(response.status)) continue;
    return response;
  }
};
