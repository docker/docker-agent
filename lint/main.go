// Package main runs project-specific linting cops using rubocop-go.
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

// cops lists every project-specific cop, in declaration order.
//
// To add a cop: declare it as a var in its own file using cop.New (or a
// cop.Func literal when it needs Scope/Types) and append it here.
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
	SlogContextual,
	ToolArgumentsViaAIJSON,
	ConstructorPurity,
	ConstructorCommandExec,
	ConstructorNetworkIO,
	WrapErrors,
	ErrorStringMatching,
	DeferMutexUnlock,
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
