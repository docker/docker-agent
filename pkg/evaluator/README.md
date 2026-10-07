# Evaluators

Evaluators return typed assessments independently of chat models and tool-approval
policies. Callers decide what a probability, choice, or score means for their use
case.

Resolve a `latest.EvaluatorConfig` against the configured providers, then construct
one reusable client per team:

```go
resolved, err := cfg.Resolve(providers)
if err != nil {
    return err
}
client, err := provider.New(ctx, resolved, env)
if err != nil {
    return err
}
result, err := client.Evaluate(ctx, map[string]any{"message": message})
```

`Evaluate` accepts a string, JSON object, or JSON array. Null, numeric, boolean,
and non-JSON-serializable states are rejected before credentials are requested.
State is sent to the configured evaluator service; callers must select the data
appropriate for that service.

## OpenAI Decisions

`openai` posts one `predicate`, `choice` or `score` question named `evaluation` to
`<base>/decisions` (default `https://api.openai.com/v1`), using `net/http` so the
OpenAI SDK stays out of the loader's dependencies. Answers are matched by name, and
probabilities, choice and score are validated strictly; refusals are ordinary errors.
Usage is read when present and otherwise unknown. There is no built-in pricing.

## Connections

Both backends go through the models gateway when one is configured, unless the
evaluator sets `bypass_models_gateway` or a custom `base_url`/`endpoint`
(`EvaluatorConfig.UsesModelsGateway`). A trusted gateway's rejected Docker token is
replayed once; every attempt is reported to the usage observer exactly once.

## TypeSafe

The initial backend posts one question named `evaluation` to `/v1/systemone`.
It requires an explicit model, uses `https://api.typesafe.ai` by default, and
retrieves `TYPESAFE_API_KEY` from the environment provider on each request. A
custom token key, base URL, exact `endpoint`, and timeout can be configured.
`endpoint` overrides the base URL and uses its path unchanged, allowing
Jev-compatible services such as Laya on Baseten's `/development/predict`.
Credentials, queries, and fragments are rejected in both URL settings. Automatic
TypeSafe pricing applies only when the effective endpoint is the official URL.
The default timeout
is 10 seconds and includes credential lookup, HTTP transfer, and response reads.
Requests respect context cancellation. Redirects are never followed, responses
are limited to 1 MiB, and errors omit credentials, input, and response bodies.
Requests are not automatically retried.

| Config type | TypeSafe question | Criteria | Result |
| --- | --- | --- | --- |
| `boolean` | `noul` | None | `Probability` in `[0, 1]` |
| `choice` | `choice` | `Choices`, a key-to-description map | `Choice` and `Probabilities` |
| `score` | `score` | `Levels`, ordered descriptions | `Score` and `Probabilities` |

Scores use zero-based level indices, with probability keys `"0"`, `"1"`, etc.
Probability maps must have exactly the configured keys and sum to one within
0.001. A selected choice must have a highest probability; ties are accepted.
Scores must lie between zero and the final level index, allowing 0.001 for
rounding. Returned values are not normalized or clamped.

`Result.Model` identifies the model returned by the service. `Result.Usage`
contains input and output token counts. Zero-valued probabilities, scores, and
confidence remain present through pointer fields. Missing or null confidence
stays `nil`; required probabilities and scores cannot be missing or null.

## Usage observation and pricing

`Result.Cost` is an estimated USD charge, with `nil` indicating unknown usage or
pricing and non-nil zero indicating a known zero charge. Automatic pricing is
limited to the official endpoint and returned model ID `jev-1.13.0`:
[$0.042/M input tokens, output free](https://docs.typesafe.ai/models). Aliases and
future versions are not guessed. Set `EvaluatorConfig.Cost` to override pricing,
including for custom endpoints; an empty cost object explicitly means free.
Overrides are copied when constructing the client. Cache prices are unused.

Observe request accounting independently of answer validity:

```go
ctx = evaluator.WithUsageObserver(ctx, func(record evaluator.UsageRecord) {
    // Record known tokens/cost or track unknown usage/pricing separately.
    account(record)
})
result, err := client.Evaluate(ctx, state)
```

The callback runs synchronously once per attempted HTTP request, even for errors
or invalid answers. Transport failures report unknown usage; local validation and
credential failures before a request produce no record.
`UsageRecord.Model` is the returned model ID, falling back to the requested ID.
`UsageRecord.Usage` is nil for missing, null, incomplete, or malformed counts;
explicit zero counts are non-nil. Unreadable or malformed responses report unknown
usage. `Result.Usage` remains a value for compatibility. The observer receives
independent copies of usage and cost; it cannot change the returned result.
Observers are context-scoped, not retained by clients. A new observer replaces
an inherited one; nil disables it. Shared callbacks must synchronize their state,
and all callbacks must finish before `Evaluate` returns. Report every attempted
request separately, including requests made by custom retrying evaluators.

Count records once, not again from results. The runtime uses these estimates for
session totals and budget accounting but are not invoices; unknown values should
not be treated as free.

Run local tests with `go test -race ./pkg/evaluator/...`. Tests use fake
environment providers and local HTTP servers, not the TypeSafe API.
