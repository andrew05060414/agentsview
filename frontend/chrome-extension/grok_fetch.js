globalThis.grokFetch = async function grokFetch(url) {
  const request = new URL(url);
  if (request.origin !== "https://grok.com") throw new Error("Unsupported Grok origin");
  if (
    request.pathname !== "/rest/app-chat/conversations" &&
    !/^\/rest\/app-chat\/conversations\/[a-f0-9-]{36}\/response-node$/i.test(request.pathname)
  )
    throw new Error("Unsupported Grok read");
  const response = await globalThis.claudeFetch(url);
  if (response.status !== 200 || request.pathname === "/rest/app-chat/conversations")
    return response;
  const tree = JSON.parse(response.body);
  if (!Array.isArray(tree.responseNodes)) throw new Error("Grok history format changed");
  const ids = tree.responseNodes.map((node) => node.responseId);
  if (ids.some((id) => typeof id !== "string" || !/^[a-f0-9-]{36}$/i.test(id)))
    throw new Error("Grok history contains an invalid response ID");
  const responses = [];
  // Keep each read bounded; never ask the conversation-generation endpoint.
  for (let offset = 0; offset < ids.length; offset += 100) {
    const loaded = await globalThis.claudeFetch(
      `${request.origin}${request.pathname.replace(/\/response-node$/, "/load-responses")}`,
      { "content-type": "application/json" },
      { method: "POST", body: JSON.stringify({ responseIds: ids.slice(offset, offset + 100) }) },
    );
    if (loaded.status !== 200) return loaded;
    const page = JSON.parse(loaded.body);
    if (!Array.isArray(page.responses)) throw new Error("Grok response format changed");
    responses.push(...page.responses);
    if (new TextEncoder().encode(JSON.stringify(responses)).byteLength > 32 * 1024 * 1024)
      return { status: 413, body: "" };
  }
  return { status: 200, body: JSON.stringify({ ...tree, responses }) };
};
