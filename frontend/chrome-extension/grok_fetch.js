// worker.js injects claude_fetch.js first. Grok reads its REST API with the
// page's cookies; the allowed request shapes are plain GETs.
async function grokFetch(url) {
  const response = await globalThis.claudeFetch(url, { Accept: "application/json" });
  if (new URL(url).pathname !== "/rest/subscriptions") return response;
  if (response.status !== 200) return { status: response.status, body: "" };
  try {
    const { userId } = JSON.parse(response.body);
    const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;
    return await globalThis.browserAccountReply(uuid.test(userId) ? userId : "");
  } catch {
    return globalThis.browserAccountReply("");
  }
}
