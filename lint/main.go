// Package main runs shared and project-specific linting cops using rubocop-go.
//
// Usage: go run ./lint [path...]
package main

import (
	"fmt"
	"os"

	"github.com/dgageot/rubocop-go/config"
	"github.com/dgageot/rubocop-go/cop"
	rubocops "github.com/dgageot/rubocop-go/cops"
	"github.com/dgageot/rubocop-go/prog"
	"github.com/dgageot/rubocop-go/runner"
)

// Select shared cops explicitly so dependency upgrades cannot enable new rules.
// Project-specific cops remain declared in this package.
var cops = []cop.Cop{
	ConfigVersionImport,
	ConfigPackageName,
	ConfigVersionConstant,
	LatestImportsPredecessor,
	ConfigLatestTagConsistency,
	ConfigVersionsRegistered,
	TUIViewPurity,
	TUIKeyBindings,
	RuntimeEventRegistry,
	RuntimeSessionScoped,
	HookConfigSync,
	HookBuiltinsRegistered,
	HookBuiltinsDocumented,
	rubocops.NewLintSlogContextual(),
	ToolArgumentsViaAIJSON,
	rubocops.NewLintConstructorPurity(),
	ConstructorCommandExec,
	rubocops.NewLintConstructorNetworkIO(),
	rubocops.NewLintWrapErrors(),
	rubocops.NewLintErrorStringMatching(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintDeferMutexUnlock(),
	rubocops.NewLintNewExpr(cop.WithScope(outsideFrozenConfig)),
	EnvironmentVariablePrefix,
	NoStdoutInLibraries,
	OTelTracerName,
	AtomicStateWrite,
	HTTPClientTransport,
	ToolsetSchemaRegistrySync,
	StatePathViaPathsPackage,
}

// programCops lists whole-program, inter-procedural cops. These run once over
// the entire loaded program rather than once per file.
var programCops = []prog.Cop{
	SlicesClone,
	prog.FromFile(SortStableFunc),
	rubocops.NewLintPointerHelper(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintReflectFields(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintStdlibUUID(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintURLClone(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintJSONMarshalWrite(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintBenchmarkLoop(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintSplitTrimJoin(),
	rubocops.NewLintFieldsSeq(),
	CutPrefix,
	CutSuffix,
	FieldsSeqLookup,
	SlicesConcat,
	SessionStateAccessors,
	rubocops.NewLintStreamCloseSafety(),
	ExclusiveStreamLease,
	DrainRunStreamBeforeRelease,
	rubocops.NewLintContextConnectivity(),
}

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = []string{"."}
	}

	r := runner.New(cops, config.DefaultConfig(), os.Stdout).
		WithProgramCops(programCops)
	offenseCount, err := r.Run(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if offenseCount > 0 {
		os.Exit(1)
	}
}
