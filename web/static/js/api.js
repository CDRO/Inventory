// The one fetch wrapper every page and component uses to talk to the backend
// (docs/specs/05-frontend-pwa-foundations.md).
//
// All requests are same-origin relative paths — Traefik serves the UI and the
// API from one origin (docs/specs/01-architecture-and-deployment.md), so
// there is no base-URL configuration here and nothing to get wrong across
// environments.

/**
 * ApiError is thrown for every non-2xx response. It carries the error
 * envelope's machine-readable pieces (docs/specs/04-backend-api-conventions.md)
 * so a caller can branch on `code` rather than parsing `message`, which is
 * human-readable fallback text and may change wording.
 */
export class ApiError extends Error {
  /**
   * @param {number} status - HTTP status code.
   * @param {string} code - stable snake_case error code, e.g. "not_found".
   * @param {string} message - human-readable fallback text.
   * @param {Object<string, string[]>} [fields] - present on 422 responses.
   */
  constructor(status, code, message, fields) {
    super(message || code);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.fields = fields || null;
  }
}

// index.html is the one page that never redirects itself away on 401 — every
// other page does, which is why the constant lives here rather than being
// repeated at each call site.
const LOGIN_PAGE = "/index.html";

/**
 * apiFetch issues one request and returns the parsed JSON body on success.
 *
 * `credentials: "same-origin"` is set unconditionally so the session cookie
 * is always sent; the cookie is HttpOnly, so this module — like every other
 * script in the app — never reads or writes it directly
 * (docs/specs/03-auth-and-multi-tenancy.md).
 *
 * On a 401 the caller is redirected to the login page and the returned
 * promise never resolves for the caller to act on — there is nothing useful
 * left to do with a request whose session is gone, and every call site would
 * otherwise have to repeat the same "if 401, redirect" branch.
 *
 * This function never inspects the response for anything resembling admin
 * status: no such field exists in any API response
 * (docs/specs/03-auth-and-multi-tenancy.md), so there is nothing here to
 * branch on even by accident.
 *
 * `options.skipAuthRedirect: true` opts out of the redirect-on-401 behaviour
 * below, turning a 401 into a normal thrown ApiError instead. This exists for
 * exactly one caller: `POST /api/auth/login` itself. That endpoint is
 * unauthenticated by design and a wrong password is an expected 401 the login
 * form needs to catch and display — not a dead session to redirect away from,
 * which is what the default behaviour would otherwise do to the login page
 * while the user is looking straight at it.
 *
 * @param {string} path - a path starting with "/", e.g. "/api/auth/me".
 * @param {RequestInit & {skipAuthRedirect?: boolean}} [options]
 * @returns {Promise<any>} the parsed JSON body, or `null` for a 204.
 * @throws {ApiError}
 */
export async function apiFetch(path, { skipAuthRedirect = false, ...options } = {}) {
  // A FormData body must NOT get an explicit Content-Type: the browser sets
  // one itself, carrying the multipart boundary it generated. Setting
  // "application/json" here would silently break every image upload, because
  // the server would try to parse a multipart body as JSON.
  const isFormBody = typeof FormData !== "undefined" && options.body instanceof FormData;

  const response = await fetch(path, {
    ...options,
    credentials: "same-origin",
    headers: {
      Accept: "application/json",
      ...(options.body != null && !isFormBody ? { "Content-Type": "application/json" } : {}),
      ...options.headers,
    },
  });

  if (response.status === 204) {
    return null;
  }

  if (response.status === 401 && !skipAuthRedirect) {
    if (!location.pathname.endsWith(LOGIN_PAGE)) {
      location.assign(LOGIN_PAGE);
      // Deliberately never resolves: the page is navigating away, and a
      // caller acting on stale data during that navigation would be a bug
      // waiting to happen, not a feature.
      return new Promise(() => {});
    }
    // Already on the login page: there is no navigation to hide behind, so
    // hanging forever here would just freeze the tab with nothing to show
    // for it. Fall through and let the caller see the 401 as a normal
    // ApiError instead.
  }

  if (!response.ok) {
    throw await toApiError(response);
  }

  const contentType = response.headers.get("Content-Type") || "";
  if (!contentType.includes("application/json")) {
    return null;
  }
  return response.json();
}

/**
 * toApiError parses the error envelope from
 * docs/specs/04-backend-api-conventions.md: `{ "error": { code, message,
 * fields? } }`. A response that fails to parse as that shape — a proxy's own
 * error page, say — still becomes an ApiError, using the HTTP status text
 * instead, rather than throwing a raw SyntaxError that tells the user
 * nothing.
 *
 * @param {Response} response
 * @returns {Promise<ApiError>}
 */
async function toApiError(response) {
  try {
    const body = await response.json();
    const error = body && body.error;
    if (error && typeof error.code === "string") {
      return new ApiError(response.status, error.code, error.message, error.fields);
    }
  } catch {
    // Fall through to the generic error below.
  }
  return new ApiError(response.status, "unknown_error", response.statusText);
}

/** GET, returning the parsed JSON body. */
export function get(path, options) {
  return apiFetch(path, options);
}

/** POST a JSON body, returning the parsed JSON response. */
export function post(path, body, options) {
  return apiFetch(path, { ...options, method: "POST", body: JSON.stringify(body ?? {}) });
}

/** PATCH a JSON body, returning the parsed JSON response. */
export function patch(path, body, options) {
  return apiFetch(path, { ...options, method: "PATCH", body: JSON.stringify(body ?? {}) });
}

/** DELETE, returning the parsed JSON response (often null / 204). */
export function del(path, options) {
  return apiFetch(path, { ...options, method: "DELETE" });
}

/**
 * postForm submits a FormData body — the multipart upload path
 * (docs/specs/04-backend-api-conventions.md). The browser sets its own
 * Content-Type with the multipart boundary, so callers must not set one; this
 * function relies on apiFetch only adding a Content-Type when `options.body`
 * is JSON, which a FormData body is not.
 *
 * @param {string} path
 * @param {FormData} formData
 * @returns {Promise<any>}
 */
export function postForm(path, formData) {
  return apiFetch(path, { method: "POST", body: formData });
}
