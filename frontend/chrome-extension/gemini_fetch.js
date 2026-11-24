// worker.js injects page_fetch.js first. Gemini reads through batchexecute
// POSTs, so the two allowed request shapes carry named parameters that
// geminiFetch turns into each RPC's argument. The page's request token stays
// in the page. worker.js injects this file again before every request into
// the same isolated world, so it declares only functions: a second top-level
// const or let would make that injection a SyntaxError.

// geminiArguments returns the RPC's argument for the request's named
// parameters, or undefined for an RPC outside the two read calls.
function geminiArguments(rpc, query) {
  const pageSize = 20;
  const cursor = query.get("cursor");
  if (rpc === "MaZiqc") return cursor ? [pageSize, cursor] : [pageSize];
  if (rpc === "hNvQHb") return [query.get("chat"), pageSize, cursor || null, 1, [0], [4], null, 1];
  return undefined;
}

// geminiPageValue reads one "key":"value" string from the app page's inline data.
function geminiPageValue(html, key) {
  const start = html.indexOf(`"${key}":"`);
  if (start < 0) return "";
  let raw = "";
  for (let i = start + key.length + 4; i < html.length && html[i] !== '"'; i++) {
    raw += html[i];
    if (html[i] === "\\") raw += html[++i];
  }
  try {
    return JSON.parse(`"${raw}"`);
  } catch {
    return "";
  }
}

// geminiSession returns the request token and build identifiers for one
// Google account slot; sessions are cached per account because the open tab
// can belong to a different account than the requested slot.
async function geminiSession(account, refresh) {
  const sessions = (globalThis.__agentsviewGeminiSessions ??= new Map());
  if (!refresh && sessions.has(account)) return sessions.get(account);
  sessions.delete(account);
  const page = await globalThis.pageFetch(`${location.origin}${account}/app`);
  if (page.status !== 200) return { status: page.status, body: "" };
  const html = page.body;
  const identity = geminiPageValue(html, "obfuscatedGaiaId");
  const session = {
    at: geminiPageValue(html, "SNlM0e"),
    sid: geminiPageValue(html, "FdrFJe"),
    bl: geminiPageValue(html, "cfb2h"),
    accountReply: await globalThis.browserAccountReply(
      /^\d{10,30}$/.test(identity) ? identity : "",
    ),
  };
  if (!session.at) return { status: 401, body: "" };
  sessions.set(account, session);
  return session;
}

async function geminiFetch(url) {
  try {
    const request = new URL(url);
    const slot = /^\/u\/([0-9])(?=\/)/.exec(request.pathname);
    if (!slot) return { status: 0, body: "", error: "Unsupported Gemini account slot" };
    const account = slot[1] === "0" ? "" : slot[0];
    if (request.pathname === `${slot[0]}/app`) {
      const session = await geminiSession(account, true);
      return session.accountReply ?? session;
    }
    const rpc = request.searchParams.get("rpcids");
    const args = geminiArguments(rpc, request.searchParams);
    if (args === undefined) return { status: 0, body: "", error: "Unsupported Gemini request" };
    for (const refresh of [false, true]) {
      const session = await geminiSession(account, refresh);
      if (!session.at) return session;
      const target = new URL(`${request.origin}${account}/_/BardChatUi/data/batchexecute`);
      target.searchParams.set("rpcids", rpc);
      target.searchParams.set("source-path", "/app");
      target.searchParams.set("_reqid", String(100000 + Math.floor(Math.random() * 900000)));
      target.searchParams.set("rt", "c");
      if (session.bl) target.searchParams.set("bl", session.bl);
      if (session.sid) target.searchParams.set("f.sid", session.sid);
      const body = new URLSearchParams();
      body.set("f.req", JSON.stringify([[[rpc, JSON.stringify(args), null, "generic"]]]));
      body.set("at", session.at);
      const response = await globalThis.pageFetch(
        target.toString(),
        { "Content-Type": "application/x-www-form-urlencoded;charset=utf-8", "X-Same-Domain": "1" },
        body.toString(),
      );
      // A refused token is fetched again once; a second refusal reaches Sync.
      if (refresh || (response.status !== 400 && response.status !== 401)) {
        if (response.status === 400 || response.status === 401)
          globalThis.__agentsviewGeminiSessions.delete(account);
        return response;
      }
    }
  } catch (error) {
    return { status: 0, body: "", error: String(error) };
  }
}
