# HTTP tool behavior in the browser replays

The HTTP replays compare model requests (including the message after a tool
call), events, and results. `httpReplay` also captures each service request's
URL, method, visible headers, and body. A failed fetch can make a robots.txt
request without fetching the page; a matching tool result alone can't prove
that behavior. The service is a Fetch API fixture, not an external web server.

- `fetch` negotiates `text`, `markdown`, and `html` case-sensitively. A missing
  or unknown format uses `Accept: text/plain;q=1.0, */*;q=0.1` and returns
  the body without HTML conversion. Nonpositive timeouts inherit the toolset
  timeout (30 seconds by default). The advertised tool schema is unchanged.
- A single URL produces a message; batches produce ordered JSON, including
  per-URL errors and repeated URLs. The byte count is for the body *after*
  any HTML conversion. HTTP error statuses are successful fetches, but
  `api` reports statuses >= 400 as tool errors. Both use Go's status names
  rather than the wire reason phrase. API output truncates at 30,000 **bytes**,
  despite the Go notice saying “character limit”.

## Intentional boundaries and known differences

- Portable keeps the eight-URL and 300-second ceilings, URL length check,
  bounded reads, and HTML conversion limits. The fetch body cap is 1 MiB;
  the session's separate redaction budget can reject a near-limit result
  including its envelope. The API reader cancels after 30,001 bytes, so it
  does not observe a broken stream *after* the truncation point as Go can.
- Direct access still requires `allow_private_ips`; otherwise `toolProxy` is
  mandatory. Direct redirects are refused. Relayed redirects are bounded,
  domain-checked, and never regain stripped headers, even on a return hop.
  Portable omits ambient credentials and referrers, rejects forbidden headers
  and userinfo, strips fragments, and pins the expanded API origin. It does
  not quote an env-expanded endpoint in a transport error. Body-read errors
  during timeouts can differ: the browser aborts the portable stream; Go can
  observe a later stream error instead.
- The robots cache is per **origin**, not Go's literal `URL.Host`: spelling
  variants of one origin share rules; HTTP and HTTPS never do. A missing
  file (404) permits the page. An unreachable, redirected unsafely, oversized,
  or unsupported file does not. Portable checks the decoded original path
  and any browser-normalized paths. CIDR domain patterns are not supported.
- Markdown conversion is offline and limited to supported markup; e.g.
  nested lists, tables, `<pre>` and malformed markup are not repaired.
  Neither response decoder transcodes according to a declared charset.
  For a raw single fetch or API result, Go can keep invalid UTF-8 in history
  while emitting a differently decoded JS event (including a stripped leading
  BOM for API output). Portable has one string for both. Invalid UTF-8 in
  Markdown also need not have the same post-conversion byte count.
