# API tool arguments in Go small and portable

`api_config.args` and `required` describe the tool to the model. They are not a
validator for model input. For example, advertising a numeric argument on GET
is valid, but sending a JSON number for it is not.

| Method | Decoding and request |
| --- | --- |
| GET | Decode a string map. `null` values become empty strings. Substitute exact key names into the endpoint, even for undeclared arguments. A missing placeholder stays literal. Do not expand the arguments themselves. An empty argument string skips this expansion. |
| POST | Decode a JSON object (or `null`) and marshal it like Go: numbers are float64, keys are sorted recursively, strings are HTML-escaped. Empty input sends `{}`, while `null` sends `null`. The endpoint is not interpolated with arguments. |
| Other admitted methods | No request body or argument expansion. |

The `args_api*` browser replays compare tool definitions, calls, confirmations,
results, and the service's view of the request. The portable host still requires
the configured egress policy and checks the expanded origin. Declaring a schema
or passing extra arguments never grants permission to send a POST.

Declarations may use string, integer, number and boolean properties, non-null scalar
`enum` lists, arrays with `items`, and objects with named properties. Portable
rejects unsupported schema keywords rather than dropping them. The host also
retains its byte and complexity limits, never forwards a fragment, and avoids
quoting an env-expanded endpoint in URL errors.

See [HTTP tool behavior](http-behavior.md) for response handling and browser limits.
