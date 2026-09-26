/**
 * router-session: send the Pi session id to the LLM router on every
 * provider request as `X-Session-Id`, so the router's request log
 * (router_requests.session_id), tokenator's gateway pairing and the pitf
 * session jumps can attribute Pi traffic to its session.
 *
 * The router reads X-Session-Id (llm-router-go internal/router/callersession.go);
 * the id is the `id` of the session file's first line. Harmless to other
 * providers. Retries reuse the headers, so this runs once per request.
 *
 * Install: copy (or symlink) into ~/.pi/agent/extensions/.
 */

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

export default function (pi: ExtensionAPI) {
	pi.on("before_provider_headers", (event, ctx) => {
		const id = ctx.sessionManager.getSessionId();
		if (id) event.headers["X-Session-Id"] = id;
	});
}
