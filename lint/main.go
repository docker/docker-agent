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
	ConfigVersionBoundary,
	ConfigLatestTagConsistency,
	ConfigVersionsRegistered,
	TUIViewPurity,
	TUIKeyBindings,
	RuntimeSessionScoped,
	HookConfigSync,
	HookBuiltinsRegistered,
	HookBuiltinsDocumented,
	rubocops.NewLintHTTPRequestWithContext(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintNoFatalOutsideMain(),
	rubocops.NewLintSlogContextual(),
	ToolArgumentsViaAIJSON,
	rubocops.NewLintConstructorPurity(),
	runner.WithDirectoryTypes(rubocops.NewLintConstructorCommandExecFile()),
	rubocops.NewLintConstructorNetworkIO(),
	rubocops.NewLintWrapErrors(),
	rubocops.NewLintErrorStringMatching(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintDeferMutexUnlock(),
	rubocops.NewLintNewExpr(cop.WithScope(outsideFrozenConfig)),
	EnvironmentVariablePrefix,
	rubocops.NewLintNoStdoutInLibraries(cop.WithScope(cop.UnderDir("pkg"))),
	OTelTracerName,
	AtomicStateWrite,
	HTTPClientTransport,
	ToolsetSchemaRegistrySync,
	StatePathViaPathsPackage,
}

// programCops lists whole-program, inter-procedural cops. These run once over
// the entire loaded program rather than once per file.
var programCops = []prog.Cop{
	rubocops.NewLintErrorsAsType(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintMapsCopy(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintMapsClone(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintSlicesContains(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintSlicesEqual(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintSplitSeq(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintSortedMapKeys(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintWaitGroupGo(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintHTTPTestRequestWithContext(cop.WithScope(outsideFrozenConfig)),
	prog.FromFileWithTests(rubocops.NewLintSlicesCloneFile(cop.WithScope(outsideFrozenConfig))),
	rubocops.NewLintSortStableFunc(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintPointerHelper(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintReflectFields(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintStdlibUUID(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintURLClone(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintJSONMarshalWrite(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintBenchmarkLoop(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintSplitTrimJoin(),
	rubocops.NewLintFieldsSeq(),
	rubocops.NewLintCutPrefix(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintCutSuffix(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintFieldsSeqLookup(),
	rubocops.NewLintSlicesConcat(cop.WithScope(outsideFrozenConfig)),
	ChannelDirection,
	// SplitGoroutineLifetime, // Enable after migrating the remaining split-lifecycle APIs.
	rubocops.NewLintCancellationLoop(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintChannelStateCheck(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintClosedChannelLoop(cop.WithScope(outsideFrozenConfig)),
	SessionStateAccessors,
	rubocops.NewLintCancellationSafeResultSend(cop.WithScope(outsideFrozenConfig)),
	rubocops.NewLintNonblockingChannelReplacement(cop.WithScope(outsideFrozenConfig)),
	BoundedCommandPipeLifetime,
	SilentLimitedRead,
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
		WithProgramCops(programCops).
		WithSharedProgramLoad()
	offenseCount, err := r.Run(paths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if offenseCount > 0 {
		os.Exit(1)
	}
}
