// OpenRouter PKCE OAuth helpers for the docker-agent wasm browser demo.
//
// Implements the flow described at:
//   https://openrouter.ai/docs/use-cases/oauth-pkce
//
// Lifecycle:
//   1. signIn():
//        - generates a 64-byte random code verifier (base64url),
//        - derives the S256 code challenge,
//        - stashes the verifier in sessionStorage,
//        - redirects the tab to https://openrouter.ai/auth?...
//   2. handleCallback() (call once on every page load):
//        - if the URL has a ?code= param, POST it together with the stashed
//          verifier to https://openrouter.ai/api/v1/auth/keys, store the
//          returned API key in localStorage, clean the URL.
//        - returns the freshly minted key, or null if there was no callback.
//   3. getKey() / signOut():
//        - read / clear the persisted key.
//
// The persisted key is a regular OpenRouter user-controlled API key. It is
// scoped, revocable from https://openrouter.ai/settings/keys, and lives only
// in the user's browser — there is no backend.
//
// Why localStorage for the key but sessionStorage for the verifier?
//   - The verifier is a one-shot secret that must survive the redirect to
//     OpenRouter and back; sessionStorage is the right scope.
//   - The key is meant to persist across reloads so the user doesn't re-auth
//     every time. localStorage matches that. This is a deliberate trade-off:
//     anything in localStorage is reachable by other scripts on the same
//     origin (XSS), so we treat the key as "user-equivalent credential" and
//     remind the user they can revoke it at the OpenRouter dashboard.

"use strict";

const STORAGE_KEY = "openrouter_api_key";
const VERIFIER_KEY = "openrouter_pkce_verifier";

// base64url-encodes an ArrayBuffer (or Uint8Array) per RFC 4648 §5.
function base64url(buf) {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  // String.fromCharCode is fine for the byte sizes we use here (<= 64 bytes
  // for the verifier, 32 for the SHA-256 digest), so no chunking needed.
  let s = "";
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// randomVerifier returns a fresh PKCE code_verifier. RFC 7636 mandates
// 43..128 characters drawn from the URL-safe alphabet; 64 bytes of randomness
// → ~86 chars after base64url, well within the range.
function randomVerifier() {
  const bytes = new Uint8Array(64);
  crypto.getRandomValues(bytes);
  return base64url(bytes);
}

// s256 hashes the verifier with SHA-256 and base64url-encodes the digest.
async function s256(verifier) {
  const data = new TextEncoder().encode(verifier);
  const digest = await crypto.subtle.digest("SHA-256", data);
  return base64url(digest);
}

// callbackURL is the URL OpenRouter redirects back to after the user grants
// access. Whatever path the page is currently served from will do; we strip
// any query/hash so the redirect target is canonical.
function callbackURL() {
  return location.origin + location.pathname;
}

// signIn kicks off the PKCE flow. The function does not return — the tab
// navigates away to OpenRouter.
export async function signIn() {
  const verifier = randomVerifier();
  sessionStorage.setItem(VERIFIER_KEY, verifier);
  const challenge = await s256(verifier);

  const url = new URL("https://openrouter.ai/auth");
  url.searchParams.set("callback_url", callbackURL());
  url.searchParams.set("code_challenge", challenge);
  url.searchParams.set("code_challenge_method", "S256");

  location.assign(url.toString());
}

// handleCallback completes the PKCE flow if the page was loaded with a `code`
// query parameter. It removes the param from the URL on success so a refresh
// doesn't re-attempt the exchange (codes are single-use) and so the key isn't
// leaked into browser history beyond the redirect itself.
//
// Returns the new API key on a successful exchange, null if there was no
// callback in progress, or throws on failure.
export async function handleCallback() {
  const params = new URLSearchParams(location.search);
  const code = params.get("code");
  if (!code) return null;

  const verifier = sessionStorage.getItem(VERIFIER_KEY);
  sessionStorage.removeItem(VERIFIER_KEY);

  // Always clean the URL, even on failure, so a stale ?code= doesn't haunt
  // future loads.
  history.replaceState({}, "", callbackURL());

  if (!verifier) {
    throw new Error("PKCE verifier missing — please click Sign in again.");
  }

  const r = await fetch("https://openrouter.ai/api/v1/auth/keys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      code,
      code_verifier: verifier,
      code_challenge_method: "S256",
    }),
  });
  if (!r.ok) {
    const body = await r.text().catch(() => "");
    throw new Error(`OpenRouter token exchange failed: ${r.status} ${body}`);
  }
  const { key } = await r.json();
  if (!key) throw new Error("OpenRouter token exchange returned no key");
  localStorage.setItem(STORAGE_KEY, key);
  return key;
}

// getKey returns the stored API key, or null if the user is not signed in.
export function getKey() {
  return localStorage.getItem(STORAGE_KEY);
}

// signOut clears the local copy of the key. The key itself remains live on
// the OpenRouter side until the user revokes it from
// https://openrouter.ai/settings/keys — there is no remote logout endpoint.
export function signOut() {
  localStorage.removeItem(STORAGE_KEY);
}
