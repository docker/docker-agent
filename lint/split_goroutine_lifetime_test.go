package main

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/coptest"
	"github.com/dgageot/rubocop-go/prog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const lifetimeFixture = `package fixture
import (
    "context"
    "sync"
)
type Worker struct {
    cancel context.CancelFunc
    done, stop chan struct{}
    wg sync.WaitGroup
    conn *connection
    count int
}
type connection struct{}
func (*connection) Read() {}
func (*connection) Close() {}
var _ = context.Background
`

func TestSplitGoroutineLifetime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{
			name: "cancel pair",
			body: `func (w *Worker) Begin(ctx context.Context) { ctx, w.cancel = context.WithCancel(ctx); go func(){ <-ctx.Done() }() }
func (w *Worker) End() { w.cancel() }`, want: 1,
		},
		{
			name: "local cancel stored later",
			body: `func (w *Worker) Begin(ctx context.Context) { ctx, cancel := context.WithCancel(ctx); w.cancel = cancel; go func(){ <-ctx.Done() }() }
func (w *Worker) End() { cancel := w.cancel; cancel() }`, want: 1,
		},
		{
			name: "wait group go",
			body: `func (w *Worker) Begin() { w.wg.Go(func(){ w.conn.Read() }) }
func (w *Worker) End() { w.wg.Wait() }`, want: 1,
		},
		{
			name: "done in worker",
			body: `func (w *Worker) Begin() { w.wg.Add(1); go func(){ defer w.wg.Done(); w.conn.Read() }() }
func (w *Worker) End() { w.wg.Wait() }`, want: 1,
		},
		{
			name: "receiver channel",
			body: `func (w *Worker) Watch() { w.stop = make(chan struct{}); go w.loop() }
func (w *Worker) loop() { <-w.stop }
func (w *Worker) End() { close(w.stop) }`, want: 1,
		},
		{
			name: "resource stored through local",
			body: `func (w *Worker) Open() { conn := new(connection); w.conn = conn; go conn.Read() }
func (w *Worker) End() { w.conn.Close() }`, want: 1,
		},
		{
			name: "same receiver helper chain",
			body: `func (w *Worker) Begin() { w.prepare() }
func (w *Worker) prepare() { w.spawn() }
func (w *Worker) spawn() { go w.loop() }
func (w *Worker) loop() { <-w.stop }
func (w *Worker) End() { w.cleanup() }
func (w *Worker) cleanup() { close(w.stop) }`, want: 1,
		},
		{
			name: "receiver alias",
			body: `func (w *Worker) Begin() { other := w; go func(){ <-other.stop }() }
func (w *Worker) End() { other := w; close(other.stop) }`, want: 1,
		},
		{
			name: "recursive helper",
			body: `func (w *Worker) Begin() { w.spawn() }
func (w *Worker) spawn() { if w.count > 0 { w.spawn() }; go func(){ <-w.stop }() }
func (w *Worker) End() { close(w.stop) }`, want: 1,
		},
		{
			name: "conditional wait",
			body: `func (w *Worker) Begin(join bool) { w.wg.Go(func(){}); if join { w.wg.Wait() } }
func (w *Worker) End() { w.wg.Wait() }`, want: 1,
		},
		{
			name: "early return before join",
			body: `func (w *Worker) Begin(early bool) { w.wg.Go(func(){}); if early { return }; w.wg.Wait() }
func (w *Worker) End() { w.wg.Wait() }`, want: 1,
		},
		{
			name: "wait before launch",
			body: `func (w *Worker) Begin() { w.wg.Wait(); w.wg.Go(func(){}) }
func (w *Worker) End() { w.wg.Wait() }`, want: 1,
		},
		{
			name: "cancellation is not a join",
			body: `func (w *Worker) Begin(ctx context.Context) { ctx, w.cancel = context.WithCancel(ctx); defer w.cancel(); go func(){ <-ctx.Done() }() }
func (w *Worker) End() { w.cancel() }`, want: 1,
		},
		{
			name: "value and pointer receiver",
			body: `func (w Worker) Begin() { go func(){ <-w.stop }() }
func (w *Worker) End() { close(w.stop) }`, want: 1,
		},
		{
			name: "synchronous run",
			body: `func (w *Worker) Run(ctx context.Context) { <-ctx.Done() }
func (w *Worker) End() { w.cancel() }`,
		},
		{
			name: "resource without background work",
			body: `func (w *Worker) Open() { w.conn = new(connection) }
func (w *Worker) End() { w.conn.Close() }`,
		},
		{
			name: "unrelated cancellation",
			body: `func (w *Worker) Begin() { go func(){ w.count++ }() }
func (w *Worker) End() { w.cancel() }`,
		},
		{
			name: "unrelated receiver",
			body: `func (w *Worker) Begin(other *Worker) { go other.loop() }
func (w *Worker) loop() { <-w.stop }
func (w *Worker) End() { close(w.stop) }`,
		},
		{
			name: "dormant closure",
			body: `func (w *Worker) Factory() func() { return func(){ go func(){ <-w.stop }() } }
func (w *Worker) End() { close(w.stop) }`,
		},
		{
			name: "fake wait group go",
			body: `type fakeGroup struct{}
func (fakeGroup) Go(f func()) { f() }
func (w *Worker) Begin() { fakeGroup{}.Go(func(){ <-w.stop }) }
func (w *Worker) End() { close(w.stop) }`,
		},
		{
			name: "joined wait group",
			body: `func (w *Worker) Run() { w.wg.Go(func(){}); w.wg.Wait() }
func (w *Worker) End() { w.wg.Wait() }`,
		},
		{
			name: "deferred join",
			body: `func (w *Worker) Run() { defer w.wg.Wait(); w.wg.Go(func(){}) }
func (w *Worker) End() { w.wg.Wait() }`,
		},
		{
			name: "helper joined by caller",
			body: `func (w *Worker) Run() { w.spawn(); w.wg.Wait() }
func (w *Worker) spawn() { w.wg.Go(func(){}) }
func (w *Worker) End() { w.wg.Wait() }`,
		},
		{
			name: "no normal return",
			body: `func (w *Worker) Run() { go func(){ <-w.stop }(); for {} }
func (w *Worker) End() { close(w.stop) }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			offenses := runLifetimeCop(t, map[string]string{"sample.go": lifetimeFixture + tc.body})
			require.Len(t, offenses, tc.want)
			for _, offense := range offenses {
				assert.Equal(t, "Lint/SplitGoroutineLifetime", offense.CopName)
				assert.Contains(t, offense.Message, "blocking Run(ctx)")
			}
		})
	}
}

func runLifetimeCop(t *testing.T, sources map[string]string) []cop.Offense {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	for name, source := range sources {
		file, err := parser.ParseFile(fset, name, source, parser.ParseComments)
		require.NoError(t, err)
		files = append(files, file)
	}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	config := &types.Config{Importer: importer.Default()}
	pkg, err := config.Check("fixture", fset, files, info)
	require.NoError(t, err)
	pass := &prog.Pass{Cop: SplitGoroutineLifetime, Program: &prog.Program{
		Fset: fset, Packages: []*packages.Package{{Syntax: files, Types: pkg, TypesInfo: info}},
	}}
	SplitGoroutineLifetime.Check(pass)
	return pass.Offenses()
}

func TestSplitGoroutineLifetimeAcrossFiles(t *testing.T) {
	t.Parallel()
	offenses := runLifetimeCop(t, map[string]string{
		"worker.go": lifetimeFixture + `func (w *Worker) Begin() { go w.loop() }`,
		"loop.go": `package fixture
func (w *Worker) loop() { <-w.stop }
func (w *Worker) End() { close(w.stop) }`,
	})
	require.Len(t, offenses, 1)
	assert.Equal(t, "worker.go", offenses[0].Pos.Filename)
	assert.Contains(t, offenses[0].Message, "stop")
}

func TestSplitGoroutineLifetimeSkipsTestsAndGenerated(t *testing.T) {
	t.Parallel()
	body := lifetimeFixture + `func (w *Worker) Begin() { go func(){ <-w.stop }() }
func (w *Worker) End() { close(w.stop) }`
	for _, tc := range []struct{ name, prefix string }{
		{"sample_test.go", ""},
		{"sample.go", "// Code generated by fixture. DO NOT EDIT.\n"},
	} {
		assert.Empty(t, runLifetimeCop(t, map[string]string{tc.name: tc.prefix + body}))
	}
}

func TestSplitGoroutineLifetimeSuppressions(t *testing.T) {
	testCorrectnessCopSuppressions(t, SplitGoroutineLifetime, lifetimeFixture+`
func (w *Worker) Begin() { go func(){ <-w.stop }() } // offense
func (w *Worker) End() { close(w.stop) }
`)
}

func TestSplitGoroutineLifetimeResolvesImports(t *testing.T) {
	t.Parallel()
	offenses := coptest.RunProgram(t, SplitGoroutineLifetime, coptest.ProgramFiles{
		"go.mod": "module example.test\n\ngo 1.27\n",
		"worker.go": lifetimeFixture + `func (w *Worker) Begin() { w.wg.Go(func(){}) }
func (w *Worker) End() { w.wg.Wait() }`,
	})
	require.Len(t, offenses, 1)
}

func TestSplitGoroutineLifetimeAdversarial(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"nonblocking_select_not_join", `func(w *Worker) Run(){ go func(){ defer close(w.done); <-w.stop }(); select {case <-w.done: default:} }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"select_other_case_not_join", `func(w *Worker) Run(ctx context.Context){ go func(){ defer close(w.done); <-w.stop }(); select {case <-w.done: case <-ctx.Done():} }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"same_channel_select_not_join", `func(w *Worker) Run(){go func(){<-w.stop}(); select {case <-w.stop: default:} }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"waitgroup_read_not_membership", `func(w *Worker) Run(){go func(){w.wg.Wait(); <-w.stop}(); w.wg.Wait() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"waitgroup_address_not_membership", `var sink any; func(w *Worker) Run(){go func(){sink = &w.wg; <-w.stop}(); w.wg.Wait() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"receiver_alias_reassigned", `func(w *Worker) Begin(other *Worker){a:=w; a=other; go a.loop() }; func(w *Worker) loop(){<-w.stop}; func(w *Worker) End(){close(w.stop)}`, 0},
		{"channel_alias_reassigned", `func(w *Worker) Begin(){ch:=w.stop; ch=make(chan struct{}); go func(){<-ch}() }; func(w *Worker) End(){close(w.stop)}`, 0},
		{"join_alias_reassigned", `func(w *Worker) Run(){w.wg.Go(func(){<-w.stop}); g:=&w.wg; g=new(sync.WaitGroup); g.Wait() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"iife_join_conditional", `func(w *Worker) Run(join bool){w.wg.Go(func(){<-w.stop}); func(){if join{w.wg.Wait()}}() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"iife_defer_before_launch", `func(w *Worker) Run(){func(){defer w.wg.Wait()}(); w.wg.Go(func(){<-w.stop}) }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"iife_early_return", `func(w *Worker) Run(early bool){w.wg.Go(func(){<-w.stop}); func(){if early{return}; w.wg.Wait()}() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"helper_performs_join", `func(w *Worker) Run(){w.wg.Go(func(){}); w.join() }; func(w *Worker) join(){w.wg.Wait()}; func(w *Worker) End(){w.wg.Wait()}`, 0},
		{"helper_chain_stop", `func(w *Worker) Begin(){go w.loop()}; func(w *Worker) loop(){ w.wait() }; func(w *Worker) wait(){<-w.stop}; func(w *Worker) End(){close(w.stop)}`, 1},
		{"channel_tail_assignment", `func(w *Worker) Begin(){go func(){_,ok:=<-w.stop; _=ok}() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"channel_tail_if", `func(w *Worker) Begin(){go func(){if w.count==0{<-w.stop}}() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"channel_loop_break", `func(w *Worker) Begin(){go func(){for{select{case <-w.stop: return; default: w.count++}}}() }; func(w *Worker) End(){close(w.stop)}`, 1},
		{"generic_receiver", `type G[T any] struct{stop chan struct{}}; func(w *G[T]) Begin(){go func(){<-w.stop}()}; func(w *G[T]) End(){close(w.stop)}`, 1},
		{"generic_method_worker", `type G[T any] struct{stop chan struct{}}; func(w *G[T]) Begin(){go w.loop()}; func(w *G[T]) loop(){<-w.stop}; func(w *G[T]) End(){close(w.stop)}`, 1},
		{"parenthesized_waitgroup_callback", `func(w *Worker) Run(){w.wg.Go((func(){<-w.stop}))}; func(w *Worker) End(){close(w.stop)}`, 1},
		{"goroutine_argument_is_synchronous", `func consume(int){}; func(w *Worker) value()int{w.conn.Read();return 1}; func(w *Worker) Begin(){go consume(w.value())}; func(w *Worker) End(){w.conn.Close()}`, 0},
		{"nested_goroutine_not_joined", `func(w *Worker) Run(){w.wg.Go(func(){go func(){<-w.stop}()});w.wg.Wait()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"unrelated_receive_is_not_stopper", `func(w *Worker) Begin(){go func(){w.done<-struct{}{}}()};func(w *Worker) Consume(){<-w.done}`, 0},

		{"wg_nonblocking_completion_select", `func(w *Worker) Run(){w.wg.Go(func(){close(w.done);<-w.stop});select{case <-w.done:default:}};func(w *Worker) End(){w.wg.Wait()}`, 1},
		{"join_short_circuit", `func(w *Worker) Run(join bool){w.wg.Go(func(){<-w.stop});if join && func()bool{w.wg.Wait();return true}() {}};func(w *Worker) End(){w.wg.Wait()}`, 1},
		{"helper_join_deferred", `func(w *Worker) Run(){defer w.join();w.wg.Go(func(){})};func(w *Worker) join(){w.wg.Wait()};func(w *Worker) End(){w.wg.Wait()}`, 0},
		{"receiver_reassigned", `func(w *Worker) Begin(other *Worker){w=other;go w.loop()};func(w *Worker) loop(){<-w.stop};func(w *Worker) End(){close(w.stop)}`, 0},
		{"join_rebound_field", `func(w *Worker) Run(){w.wg.Go(func(){<-w.stop});w.wg=sync.WaitGroup{};w.wg.Wait()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"wg_same_type_no_control", `func(w *Worker) Run(){go func(){_ = w.wg;<-w.stop}();w.wg.Wait()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"nested_detached_in_helper", `func(w *Worker) Run(){w.wg.Go(w.spawn);w.wg.Wait()};func(w *Worker) spawn(){go func(){<-w.stop}()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"multiple_cancel_pair", `func(w *Worker) Begin(ctx context.Context){ctx,c:=context.WithCancel(ctx);w.cancel=c;ctx,c=context.WithCancel(context.Background());_ = c;go func(){<-ctx.Done()}()};func(w *Worker) End(){w.cancel()}`, 0},

		{"completion_closed_before_work", `func(w *Worker) Run(){go func(){close(w.done);<-w.stop}();<-w.done};func(w *Worker) End(){close(w.stop)}`, 1},
		{"completion_closed_conditionally", `func(w *Worker) Run(b bool){go func(){if b{close(w.done)};<-w.stop}();<-w.done};func(w *Worker) End(){close(w.stop)}`, 1},
		{"sibling_launch_join_does_not_contain_other", `func(w *Worker) Run(){w.wg.Go(func(){close(w.done)}); go func(){<-w.stop}();w.wg.Wait()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"parenthesized_go_callback", `func(w *Worker) Begin(){go (func(){<-w.stop})()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"select_default_literal_no_panic", `func(w *Worker) Begin(){go func(){select{default:return}}()};func(w *Worker) End(){close(w.stop)}`, 0},
		{"return_inside_nested_iife_not_worker_terminal", `func(w *Worker) Begin(){go func(){func(){select{case <-w.stop:return;default:return}}();for{}}()};func(w *Worker) End(){close(w.stop)}`, 0},
		{"normal_blocking_channel_join", `func(w *Worker) Run(){go func(){defer close(w.done);<-w.stop}();<-w.done};func(w *Worker) End(){close(w.stop)}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runLifetimeCop(t, map[string]string{"sample.go": lifetimeFixture + tc.body})
			t.Logf("got=%d want=%d %v", len(got), tc.want, got)
			if len(got) != tc.want {
				t.Errorf("got=%d want=%d", len(got), tc.want)
			}
		})
	}
}

func TestSplitGoroutineLifetimeDiamond(t *testing.T) {
	for _, depth := range []int{30} {
		var b strings.Builder
		b.WriteString(lifetimeFixture)
		fmt.Fprintf(&b, "func(w *Worker) Run(){w.a%d();w.b%d();w.wg.Wait()};func(w *Worker) End(){w.wg.Wait()};func(w *Worker) spawn(){w.wg.Go(func(){})};", depth, depth)
		for i := 0; i <= depth; i++ {
			for _, side := range []string{"a", "b"} {
				fmt.Fprintf(&b, "func(w *Worker) %s%d(){", side, i)
				if i == 0 {
					b.WriteString("w.spawn()")
				} else {
					fmt.Fprintf(&b, "w.a%d();w.b%d()", i-1, i-1)
				}
				b.WriteString("};")
			}
		}
		got := runLifetimeCop(t, map[string]string{"sample.go": b.String()})
		require.Empty(t, got)
	}
}

func TestSplitGoroutineLifetimeReview(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"deferred_helper_launch", `func(w *Worker) Run(){defer w.spawn();w.wg.Wait()};func(w *Worker) spawn(){w.wg.Go(func(){<-w.stop})};func(w *Worker) End(){close(w.stop)}`, 1},
		{"deferred_helper_lifo", `func(w *Worker) Run(){defer w.spawn();defer w.wg.Wait()};func(w *Worker) spawn(){w.wg.Go(func(){<-w.stop})};func(w *Worker) End(){close(w.stop)}`, 1},
		{"intermediate_helper_detached_descendant", `func(w *Worker) Run(){w.start();w.wg.Wait()};func(w *Worker) start(){w.wg.Go(w.spawn)};func(w *Worker) spawn(){go func(){<-w.stop}()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"done_before_worker_finishes", `func(w *Worker) Run(){w.wg.Add(1);go func(){w.wg.Done(); <-w.stop}();w.wg.Wait()}; func(w *Worker) End(){close(w.stop)}`, 1},
		{"method_worker_done_is_join", `func(w *Worker) Run(){w.wg.Add(1);go w.loop();w.wg.Wait()}; func(w *Worker) loop(){defer w.wg.Done(); <-w.stop}; func(w *Worker) End(){close(w.stop)}`, 0},
		{"done_inside_conditional_not_completion", `func(w *Worker) Run(b bool){w.wg.Add(1);go func(){if b{w.wg.Done()}; <-w.stop}();w.wg.Wait()}; func(w *Worker) End(){close(w.stop)}`, 1},
		{"iife_worker_misses_controls", `func(w *Worker) Begin(){go func(){func(){<-w.stop}()}()}; func(w *Worker) End(){close(w.stop)}`, 1},
		{"iife_only_done_is_false_join", `func(w *Worker) Run(){w.wg.Add(1);go func(){func(){w.wg.Done()}();<-w.stop}();w.wg.Wait()}; func(w *Worker) End(){close(w.stop)}`, 1},
		{"nested_done_is_not_outer_membership", `func(w *Worker) Run(){w.wg.Add(1);go func(){go func(){w.wg.Done()}();<-w.stop}();w.wg.Wait()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"joined_iife", `func(w *Worker) Run(){func(){w.wg.Go(func(){});w.wg.Wait()}()};func(w *Worker) End(){w.wg.Wait()}`, 0},
		{"joined_deferred_iife", `func(w *Worker) Run(){defer func(){w.wg.Go(func(){});w.wg.Wait()}()};func(w *Worker) End(){w.wg.Wait()}`, 0},
		{"deferred_launch_after_join", `func(w *Worker) Run(){defer w.wg.Go(func(){<-w.stop});w.wg.Wait()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"range_rebind_defeats_join", `func(w *Worker) Run(){w.wg.Go(func(){<-w.stop});for _,w.wg=range []sync.WaitGroup{{}}{};w.wg.Wait()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"range_rebind_receiver", `func(w *Worker) Begin(other *Worker){for _,w=range []*Worker{other}{};go w.loop()};func(w *Worker) loop(){<-w.stop};func(w *Worker) End(){close(w.stop)}`, 0},
		{"nested_wait_not_join", `func(w *Worker) Run(){w.wg.Go(func(){<-w.stop});defer func(){go w.wg.Wait()}()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"deferred_launch_lifo", `func(w *Worker) Run(){defer w.wg.Go(func(){});defer w.wg.Wait()};func(w *Worker) End(){w.wg.Wait()}`, 1},
		{"method_worker_done_without_stop", `func(w *Worker) Run(){w.wg.Add(1);go w.loop();w.wg.Wait()}; func(w *Worker) loop(){defer w.wg.Done();w.conn.Read()}; func(w *Worker) End(){w.wg.Wait()}`, 0},
		{"helper_nested_same_group_joined", `func(w *Worker) Run(){w.wg.Go(w.spawn);w.wg.Wait()};func(w *Worker) spawn(){w.wg.Go(func(){<-w.stop})};func(w *Worker) End(){close(w.stop)}`, 0},
		{"helper_membership_mixed", `func(w *Worker) Run(){w.spawn();w.wg.Wait()};func(w *Worker) spawn(){w.wg.Go(func(){});go func(){<-w.stop}()};func(w *Worker) End(){close(w.stop)}`, 1},
		{"helper_read_is_not_group_membership", `func(w *Worker) Run(){w.spawn(); w.wg.Wait()}; func(w *Worker) spawn(){go func(){w.wg.Wait(); <-w.stop}()}; func(w *Worker) End(){close(w.stop)}`, 1},
		{"nested_same_group_is_joined", `func(w *Worker) Run(){w.wg.Go(func(){w.wg.Go(func(){<-w.stop})}); w.wg.Wait()}; func(w *Worker) End(){close(w.stop)}`, 0},
		{"receiver_range_reassignment", `func(w *Worker) Begin(other *Worker){a:=w; for _,a=range []*Worker{other}{}; go a.loop()}; func(w *Worker) loop(){<-w.stop}; func(w *Worker) End(){close(w.stop)}`, 0},
		{"channel_range_reassignment", `func(w *Worker) Begin(){ch:=w.stop; for _,ch=range []chan struct{}{make(chan struct{})}{}; go func(){<-ch}()}; func(w *Worker) End(){close(w.stop)}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runLifetimeCop(t, map[string]string{"sample.go": lifetimeFixture + tc.body})
			t.Logf("offenses: %v", got)
			require.Len(t, got, tc.want)
		})
	}
}

func TestSplitGoroutineLifetimeReviewPermutation(t *testing.T) {
	decls := []string{
		`func(w *Worker) Run(){w.wg.Go(w.spawn);w.wg.Wait()};`,
		`func(w *Worker) spawn(){go func(){<-w.stop}()};`,
		`func(w *Worker) End(){close(w.stop)};`,
	}
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		var body strings.Builder
		body.WriteString(lifetimeFixture)
		for _, i := range order {
			body.WriteString(decls[i])
		}
		got := runLifetimeCop(t, map[string]string{"sample.go": body.String()})
		require.Len(t, got, 1)
		require.Contains(t, got[0].Message, "spawn launches")
	}
}

func TestSplitGoroutineLifetimeReviewGenericPermutation(t *testing.T) {
	decls := []string{
		`func(w *G[T]) Begin(){go w.loop()};`,
		`func(w *G[T]) loop(){<-w.stop};`,
		`func(w *G[T]) End(){close(w.stop)};`,
	}
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		var body strings.Builder
		body.WriteString(`package fixture; type G[T any] struct{stop chan struct{}};`)
		for _, i := range order {
			body.WriteString(decls[i])
		}
		got := runLifetimeCop(t, map[string]string{"sample.go": body.String()})
		require.Len(t, got, 1)
		require.Contains(t, got[0].Message, "Begin launches")
	}
}
