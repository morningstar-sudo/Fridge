// SCRAM-SHA-256 (RFC 5802 / 7677), no channel binding, with Web Crypto.
// The password is only ever used here, to derive keys; it is never sent.

const enc = new TextEncoder();

export const ITERATIONS = 600000;

export const b64 = (bytes) => btoa(String.fromCharCode(...new Uint8Array(bytes)));
export const unb64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

async function hmac(key, data) {
  const k = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, typeof data === "string" ? enc.encode(data) : data));
}

async function sha256(data) {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", data));
}

export async function saltedPassword(password, salt, iterations) {
  const k = await crypto.subtle.importKey("raw", enc.encode(password), "PBKDF2", false, ["deriveBits"]);
  return new Uint8Array(await crypto.subtle.deriveBits({ name: "PBKDF2", hash: "SHA-256", salt, iterations }, k, 256));
}

// Credentials the server stores: it can check a login with them but cannot recover the password.
export async function makeCredentials(password) {
  const salt = crypto.getRandomValues(new Uint8Array(16));
  const salted = await saltedPassword(password, salt, ITERATIONS);
  const clientKey = await hmac(salted, "Client Key");
  return {
    salt: b64(salt),
    iterations: ITERATIONS,
    stored_key: b64(await sha256(clientKey)),
    server_key: b64(await hmac(salted, "Server Key")),
  };
}

// clientProof turns server-first into client-final and the server signature to expect.
// username must not contain "," or "=" (account names never do).
export async function clientProof(username, password, clientNonce, serverFirst) {
  const parts = Object.fromEntries(serverFirst.split(",").map((p) => [p[0], p.slice(2)]));
  const iterations = Number(parts.i);
  if (!parts.r || !parts.r.startsWith(clientNonce) || parts.r.length === clientNonce.length || !parts.s || !/^\d+$/.test(parts.i)) {
    throw new Error("Máy chủ trả lời sai định dạng");
  }
  if (iterations < 4096 || iterations > 10000000) throw new Error("Máy chủ yêu cầu số vòng lặp không hợp lệ");
  const salted = await saltedPassword(password, unb64(parts.s), iterations);
  const clientKey = await hmac(salted, "Client Key");
  const storedKey = await sha256(clientKey);
  const withoutProof = "c=biws,r=" + parts.r;
  const authMessage = `n=${username},r=${clientNonce},${serverFirst},${withoutProof}`;
  const signature = await hmac(storedKey, authMessage);
  const proof = clientKey.map((b, i) => b ^ signature[i]);
  const serverSignature = await hmac(await hmac(salted, "Server Key"), authMessage);
  return { clientFinal: `${withoutProof},p=${b64(proof)}`, serverFinal: "v=" + b64(serverSignature) };
}

function nonce() {
  return b64(crypto.getRandomValues(new Uint8Array(18))).replace(/[+/=]/g, "x");
}

// scramLogin runs start -> finish against the two URLs and returns the finish response.
// post(url, body) -> parsed JSON (throws on HTTP errors). extra is merged into the finish body.
export async function scramLogin(post, username, password, startURL, finishURL, extra = {}) {
  const clientNonce = nonce();
  const start = await post(startURL, { client_first: `n,,n=${username},r=${clientNonce}` });
  const { clientFinal, serverFinal } = await clientProof(username, password, clientNonce, start.server_first);
  const fin = await post(finishURL, { scram_id: start.scram_id, client_final: clientFinal, ...extra });
  if (fin.server_final !== serverFinal) throw new Error("Máy chủ không chứng minh được nó biết mật khẩu — dừng lại");
  return fin;
}
