package main

import (
	"io"
	"testing"

	"github.com/dgageot/rubocop-go/config"
	"github.com/dgageot/rubocop-go/prog"
	"github.com/dgageot/rubocop-go/runner"
	"github.com/stretchr/testify/require"
)

func BenchmarkLintRepository(b *testing.B) {
	b.Chdir("..")
	b.ReportAllocs()
	for b.Loop() {
		r := runner.New(cops, config.DefaultConfig(), io.Discard).
			WithProgramCops(programCops).
			WithSharedProgramLoad()
		count, err := r.Run([]string{"."})
		require.NoError(b, err)
		require.Zero(b, count)
	}
}

func BenchmarkChannelDirection(b *testing.B) {
	program, err := prog.LoadDir("..", "./...")
	require.NoError(b, err)
	_, err = program.PackagesWithTests()
	require.NoError(b, err)
	b.ReportAllocs()
	for b.Loop() {
		pass := &prog.Pass{Cop: ChannelDirection, Program: program}
		ChannelDirection.Check(pass)
		require.Empty(b, pass.Offenses())
	}
}

func BenchmarkBoundedCommandPipeLifetime(b *testing.B) {
	program, err := prog.LoadDir("..", "./...")
	require.NoError(b, err)
	b.ReportAllocs()
	for b.Loop() {
		pass := &prog.Pass{Cop: BoundedCommandPipeLifetime, Program: program}
		BoundedCommandPipeLifetime.Check(pass)
		require.Empty(b, pass.Offenses())
	}
}

func BenchmarkSilentLimitedRead(b *testing.B) {
	program, err := prog.LoadDir("..", "./...")
	require.NoError(b, err)
	b.ReportAllocs()
	for b.Loop() {
		pass := &prog.Pass{Cop: SilentLimitedRead, Program: program}
		SilentLimitedRead.Check(pass)
		require.Empty(b, pass.Offenses())
	}
}
