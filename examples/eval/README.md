# Simple eval

This is a simple agent that has two eval sessions saved in the `evals` directory, to run the eval you can:

```console
$ docker agent eval demo.yaml ./evals
```

This will output something like

```console
Eval file: 41b179a2-ed19-4ae2-a45d-95775aaa90f7
Tool trajectory score: 1.000000
Rouge-1 score: 0.521739
Eval file: 5d83e247-061f-4462-9b2d-240facde45f3
Tool trajectory score: 1.000000
Rouge-1 score: 0.829268
```

## Evaluator judge

Set `OPENAI_API_KEY` for the agent and `TYPESAFE_API_KEY` for the host-side
judge, then run:

```console
$ docker agent eval judge-evaluator.yaml ./judge-evals --judge-type evaluator --judge-model relevance_judge
```

The `judge-evals` session asks the agent to greet the user and has explicit
relevance criteria, so both commands exercise the judge.

The named boolean evaluator receives `transcript` and `criterion`. Each criterion
passes when its probability is at least 0.5. To use the default evaluator without
an additional YAML definition:

```console
$ docker agent eval demo.yaml ./judge-evals --judge-type evaluator
```
