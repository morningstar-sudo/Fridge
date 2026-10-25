// Same-origin JSON calls. The session is an HttpOnly cookie; nothing secret is kept in JS.

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

// Public pages say so, so a 401 there is an ordinary error and not a redirect.
let onUnauthorized = () => {};
export function setUnauthorizedHandler(fn) {
  onUnauthorized = fn;
}

async function send(method, path, body, headers = {}) {
  const init = { method, credentials: "same-origin", headers: { ...headers } };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  return fetch(path, init);
}

async function fail(res, quiet) {
  let msg = `HTTP ${res.status}`;
  try {
    msg = (await res.json()).error || msg;
  } catch {
    /* not JSON */
  }
  if (res.status === 401 && !quiet) onUnauthorized(msg);
  throw new ApiError(msg, res.status);
}

// api returns the parsed JSON body (null for 204). quiet = a 401 is the caller's business (login forms).
export async function api(method, path, body, { quiet = false } = {}) {
  const res = await send(method, path, body);
  if (!res.ok) await fail(res, quiet);
  if (res.status === 204) return null;
  return res.json();
}

export async function apiText(path) {
  const res = await send("GET", path);
  if (!res.ok) await fail(res, false);
  return res.text();
}

// download saves a response body as a file.
export async function download(path, filename) {
  const res = await send("GET", path);
  if (!res.ok) await fail(res, false);
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}
