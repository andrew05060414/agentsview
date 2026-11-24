// worker.js injects page_fetch.js first. Grok reads its REST API with the
// page's cookies; the allowed request shapes are plain GETs. The
// /rest/subscriptions reply names the signed-in account, so it is answered
// with the identity hash instead of the raw subscription body.
async function grokFetch(url, headers) {
  const response = await globalThis.pageFetch(url, headers);
  if (new URL(url).pathname !== "/rest/subscriptions") return response;
  if (response.status !== 200) return { status: response.status, body: "" };
  try {
    const parsed = JSON.parse(response.body);
    // The account id sits in the first subscription's xaiUserId; userId at the
    // top level is kept for an older response shape.
    const userId =
      parsed.userId ??
      (parsed.subscriptions ?? [])
        .map((entry) => entry?.xaiUserId)
        .find((id) => typeof id === "string");
    const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;
    return await globalThis.browserAccountReply(uuid.test(userId) ? userId : "");
  } catch {
    return globalThis.browserAccountReply("");
  }
}
