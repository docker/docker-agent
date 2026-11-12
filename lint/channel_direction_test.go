package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
)

func TestChannelDirection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, src string
		want      []string
	}{
		{"send", `func f(c chan int) { c <- 1 }`, []string{"parameter c:send-only"}},
		{"close", `func f(c chan int) { close(c) }`, []string{"parameter c:send-only"}},
		{"receive", `func f(c chan int) { <-c }`, []string{"parameter c:receive-only"}},
		{"range", `func f(c chan int) { for range c {} }`, []string{"parameter c:receive-only"}},
		{"both", `func f(c chan int) { close(c); <-c }`, nil},
		{"unused", `func f(c chan int) { _ = len(c); _ = cap(c); _ = c == nil }`, nil},
		{"alias", `func f(c chan int) { x := c; <-x }`, []string{"parameter c:receive-only"}},
		{"helper", `func f(c chan int) { g(c) }; func g(d chan int) { close(d) }`, []string{"parameter c:send-only", "parameter d:send-only"}},
		{"cycle", `func f(c chan int) { g(c) }; func g(d chan int) { <-d; f(d) }`, []string{"parameter c:receive-only", "parameter d:receive-only"}},
		{"sink", `type sink struct{ c chan int }; func newSink(c chan int) *sink { return &sink{c:c} }; func (s *sink) emit() { s.c <- 1 }`, []string{"parameter c:send-only"}},
		{"sink interface", `type Sink interface{ emit() }; type sink struct{ c chan int }; func newSink(c chan int) Sink { return &sink{c:c} }; func (s *sink) emit() { s.c <- 1 }`, nil},
		{"producer", `func f() chan int { c := make(chan int); go func(){ c <- 1; close(c) }(); return c }; func g(){ <-f() }`, []string{"result:receive-only"}},
		{"passthrough", `func f(c chan int) chan int { return c }; func g(){ <-f(make(chan int)) }`, []string{"parameter c:receive-only", "result:receive-only"}},
		{"fixed receiver", `func f(c chan int) { close(c); g(c) }; func g(<-chan int) {}`, nil},
		{"explicit conversion", `func f(c chan int) { <-c; _ = (chan int)(c) }`, nil},
		{"directional conversion", `func f(c chan int) { _ = (<-chan int)(c) }`, []string{"parameter c:receive-only"}},
		{"function value", `func f(c chan int) { <-c }; var callback func(chan int) = f`, nil},
		{"method interface", `type I interface{ Read(chan int) }; type T struct{}; func (T) Read(c chan int) { <-c }; var _ I = T{}`, nil},
		{"unknown callback", `func f(c chan int, g func(chan int)) { g(c); <-c }`, nil},
		{"boxed channel", `func f(c chan int) any { <-c; return c }`, nil},
		{"channel comparison", `func f(a,b chan int) bool { close(a); <-b; return a == b }`, nil},
		{"address", `func f(c chan int) *chan int { <-c; return &c }`, nil},
		{"stored interface", `func f(c chan int) { var a any = c; _ = a; <-c }`, nil},
		{"container", `func f(c chan int) []chan int { <-c; return []chan int{c} }`, nil},
		{"channel element", `func f(dst chan chan int, c chan int) { dst <- c; <-c }`, []string{"parameter dst:send-only"}},
		{"named", `type C chan int; func f(c C) { <-c }`, nil},
		{"type alias", `type C = chan int; func f(c C) { <-c }`, nil},
		{"closure", `func f(c chan int) { go func(){ <-c }() }`, []string{"parameter c:receive-only"}},
		{"tuple", `func f() (chan int, int) { return make(chan int), 1 }; func g(){ c,_ := f(); <-c }`, []string{"result:receive-only"}},
		{"named result", `func f() (c chan int) { c = make(chan int); return }; func g(){ <-f() }`, []string{"result c:receive-only"}},
		{"generic", `func g[C ~chan int](c C){ <-c }; func f(c chan int){ g(c); <-c }`, nil},
		{"named struct conversion", `type A struct{c chan int}; type B struct{c chan int}; func f(c chan int){ <-c; b:=B(A{c}); b.c<-1 }`, nil},
		{"literal assigned", `var callback func(chan int) = func(c chan int){ <-c }`, nil},
		{"literal passed", `func g(func(chan int)){}; func f(){ g(func(c chan int){ <-c }) }`, nil},
		{"map store", `func f(c chan int, m map[string]chan int){ <-c; m["x"] = c }`, nil},
		{"pointer store", `func f(c chan int, p *chan int){ <-c; *p = c }`, nil},
		{"map key", `func f(c chan int){ <-c; _ = map[chan int]int{c:1} }`, nil},
		{"anonymous interface", `type T struct{}; func (T) Read(c chan int){ <-c }; var _ interface{Read(chan int)} = T{}`, nil},
		{"local interface", `type T struct{}; func (T) Read(c chan int){ <-c }; func f(){ type I interface{Read(chan int)}; var _ I = T{} }`, nil},
		{"tuple arguments", `func pair()(chan int,int){ return make(chan int),0 }; func send(c chan int,n int){ c<-n }; func f(){ send(pair()); c,_:=pair(); <-c }`, []string{"parameter c:send-only"}},
		{"boxed tuple return", `func pair()(chan int,int){ return make(chan int),0 }; func f()(any,int){ return pair() }; func g(){ c,_:=pair(); <-c }`, nil},
		{"boxed struct", `func f(c chan int) any { <-c; return struct{ C chan int }{c} }`, nil},
		{"anonymous struct result", `func f(c chan int) struct{ C chan int }{ <-c; return struct{ C chan int }{c} }; func g(){ f(make(chan int)).C<-1 }`, nil},
		{"method direct", `type T struct{}; func(T) f(c chan int){ <-c }; func g(c chan int){ T{}.f(c) }`, []string{"parameter c:receive-only", "parameter c:receive-only"}},
		{"method expression", `type T struct{}; func(T) f(c chan int){ <-c }; func g(c chan int){ T.f(T{},c); <-c }`, nil},
		{"aggregate escape", `type box struct{ c chan int }; func f(c chan int, g func(box)){ g(box{c:c}); <-c }`, nil},
		{"structural assignment", `type Box struct{ C chan int }; func f(c chan int){ <-c; b:=Box{c}; var out struct{ C chan int }=b; out.C<-1 }`, nil},
		{"structural pointer assignment", `type Box struct{ C chan int }; func f(c chan int){ <-c; b:=&Box{c}; var out *struct{ C chan int }=(*struct{ C chan int })(b); out.C<-1 }`, nil},
		{"structural argument", `type Box struct{ C chan int }; func g(b struct{ C chan int }){ b.C<-1 }; func f(c chan int){ <-c; g(Box{c}) }`, nil},
		{"structural return", `type Box struct{ C chan int }; func f(c chan int) struct{ C chan int }{ <-c; return Box{c} }; func g(){ f(make(chan int)).C<-1 }`, nil},
		{"anonymous aliases", `type A=struct{ C chan int }; type B=struct{ C chan int }; func f(c chan int){ <-c; a:=A{c}; var b B=a; b.C<-1 }`, nil},
		{"aggregate container element", `type Box struct{ C chan int }; func f(c chan int){ <-c; out:=[]struct{ C chan int }{Box{c}}; out[0].C<-1 }`, nil},
		{"aggregate map element", `type Box struct{ C chan int }; func f(c chan int){ <-c; out:=map[int]struct{ C chan int }{0:Box{c}}; out[0].C<-1 }`, nil},
		{"tuple interface argument", `import "reflect"; type Box struct{ C chan int }; func pair(c chan int)(Box,int){ <-c; return Box{c},0 }; func observe(v any, _ int) reflect.ChanDir { return reflect.TypeOf(v).Field(0).Type.ChanDir() }; func g(c chan int){ observe(pair(c)) }`, nil},
		{"tuple interface assignment", `type Box struct{ C chan int }; func pair(c chan int)(Box,int){ <-c; return Box{c},0 }; func g(c chan int) any { var v any; v,_=pair(c); return v }`, nil},
		{"tuple interface return", `type Box struct{ C chan int }; func pair(c chan int)(Box,int){ <-c; return Box{c},0 }; func g(c chan int)(any,int){ return pair(c) }`, nil},
		{"tuple unknown callback", `type Box struct{ C chan int }; func pair(c chan int)(Box,int){ <-c; return Box{c},0 }; func g(c chan int, cb func(Box,int)){ cb(pair(c)) }`, nil},
		{"tuple variadic argument", `type Box struct{ C chan int }; func pair(c chan int)(int,Box){ <-c; return 0,Box{c} }; func observe(_ int, vs ...any) any { return vs }; func g(c chan int){ observe(pair(c)) }`, nil},
		{"tuple variadic only", `type Box struct{ C chan int }; func pair(c chan int)(Box,int){ <-c; return Box{c},0 }; func observe(vs ...any) any { return vs }; func g(c chan int){ observe(pair(c)) }`, nil},
		{"boxed channel aggregate", `type Box struct{ C chan int }; func f(c chan int) any { <-c; q:=make(chan Box,1); q<-Box{c}; return q }`, nil},
		{"nonempty interface escape", `type Box struct{ C chan int }; func(Box) Name()string{return "box"}; type I interface{Name()string}; func f(c chan int) I { <-c; return Box{c} }`, nil},
		{"panic aggregate", `type Box struct{ C chan int }; func f(c chan int){ <-c; panic(Box{c}) }`, nil},
		{"append interface aggregate", `type Box struct{ C chan int }; func f(c chan int) []any { <-c; return append([]any{},Box{c}) }`, nil},
		{"append directional element", `func f(c chan int) []<-chan int { return append([]<-chan int{},c) }`, []string{"parameter c:receive-only"}},
		{"type-only reflection", `import "reflect"; type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; func direction() reflect.ChanDir { return reflect.TypeFor[Box]().Field(0).Type.ChanDir() }`, nil},
		{"pointer type-only reflection", `import "reflect"; type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; func direction() reflect.ChanDir { return reflect.TypeFor[*Box]().Elem().Field(0).Type.ChanDir() }`, nil},
		{"nested type-only reflection", `import "reflect"; type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; func direction() reflect.Type { return reflect.TypeFor[map[string][]Box]() }`, nil},
		{"nonempty interface argument", `type Box struct{ C chan int }; func(Box) Name() string { return "box" }; type I interface{ Name() string }; func keep(v I) I { return v }; func f(c chan int) I { <-c; return keep(Box{c}) }`, nil},
		{"unknown generic type arguments", `type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; func observe[A,B any](){}; func g(){ observe[int,Box]() }`, nil},
		{"tuple directional argument", `func pair()(chan int,int){ return make(chan int),0 }; func read(c <-chan int,_ int){ <-c }; func g(){ read(pair()) }`, []string{"result:receive-only"}},
		{"switch channel comparison", `func f(c chan int,d chan<- int)bool{ <-c; switch c { case d:return true }; return false }`, nil},
		{"switch function result", `func f(c chan int)chan int{return c}; func g(d chan<- int){ c:=f(make(chan int)); <-c; switch c { case d: } }`, nil},
		{"interface equality", `func f(c chan int,v any)bool{ <-c; return c==v }`, nil},
		{"interface switch", `func f(c chan int,v any)bool{ <-c; switch c { case v:return true }; return false }`, nil},
		{"interface switch tag", `func f(c chan int,v any)bool{ <-c; switch v { case c:return true }; return false }`, nil},
		{"append tuple", `func pair(c chan int)([]chan int,chan int){ <-c; return nil,c }; func f(c chan int){ xs:=append(pair(c)); xs[0]<-1 }`, nil},
		{"delete tuple", `type Box struct{ C chan int }; func pair(c chan int)(map[any]int,any){ <-c; return nil,Box{c} }; func f(c chan int){ delete(pair(c)) }`, nil},
		{"function type reflection", `import "reflect"; type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; func observe() reflect.Type { return reflect.TypeFor[func() Box]() }`, nil},
		{"interface method type reflection", `import "reflect"; type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; func observe() reflect.Type { return reflect.TypeFor[interface{ Get() Box }]() }`, nil},
		{"boxed function", `type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; var v any=f`, nil},
		{"inferred generic constraint", `type Box struct{ C chan int }; func accept[T ~struct{ C chan int }](v T){}; func f(c chan int){ <-c; accept(Box{c}) }`, nil},
		{"inferred generic reflection", `import "reflect"; type Box struct{ C chan int }; func accept[T any](v T) reflect.Type{return reflect.TypeOf(v)}; func f(c chan int){ <-c; accept(Box{c}) }`, nil},
		{"inferred generic type reflection", `import "reflect"; type Box struct{ C chan int }; func accept[T any](v T) reflect.Type{return reflect.TypeFor[T]()}; func f(c chan int){ <-c; accept(Box{c}) }`, nil},
		{"generic function value constraint", `type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; func accept[T ~struct{ C chan int }](){}; var invoke=accept[Box]; func g(){ invoke() }`, nil},
		{"generic function value reflection", `import "reflect"; type Box struct{ C chan int }; func f(c chan int) Box { <-c; return Box{c} }; var observe=reflect.TypeFor[Box]; func g() reflect.Type { return observe() }`, nil},
		{"switch nil", `func f(c chan int)bool{ <-c; switch c { case nil:return true }; return false }`, []string{"parameter c:receive-only"}},
	}
	files := coptest.ProgramFiles{"go.mod": "module example.test\n\ngo 1.27\n"}
	for i, tc := range tests {
		files[fmt.Sprintf("case%d/sample.go", i)] = "package p\n" + tc.src
	}
	offenses := coptest.RunProgram(t, ChannelDirection, files)
	got := make(map[string][]string)
	for _, offense := range offenses {
		label, rest, ok := strings.Cut(offense.Message, " can be ")
		assert.True(t, ok)
		direction, _, _ := strings.Cut(rest, " ")
		key := filepath.Base(filepath.Dir(offense.Pos.Filename))
		got[key] = append(got[key], label+":"+direction)
	}
	for i, tc := range tests {
		assert.ElementsMatch(t, tc.want, got[fmt.Sprintf("case%d", i)], tc.name)
	}
}

func TestChannelDirectionPackagesAndTests(t *testing.T) {
	t.Parallel()
	offenses := coptest.RunProgram(t, ChannelDirection, coptest.ProgramFiles{
		"go.mod": "module example.test\n\ngo 1.27\n",
		"sink/sink.go": `package sink
func Send(c chan int) { c <- 1 }
func Read(c chan int) { <-c }
func Both(c chan int) { <-c }
`,
		"sink/sink_test.go": `package sink
import "testing"
func TestBoth(t *testing.T) { c := make(chan int,1); c <- 1; Both(c) }
func send(c chan int) { c <- 1; Both(c) }
func testHelper(c chan int) { close(c) }
var _ func(chan int) = Both
`,
		"consumer/consumer.go": `package consumer
import "example.test/sink"
func Forward(c chan int) { sink.Send(c) }
`,
		"sink/external_test.go": `package sink_test
import "example.test/sink"
func helper(c chan int) { sink.Read(c) }
`,
		"testonly/helper_test.go": `package testonly
func helper(c chan int) { <-c }
`,
	})
	var got []string
	for _, offense := range offenses {
		got = append(got, filepath.Base(offense.Pos.Filename)+":"+strconv.Itoa(offense.Pos.Line))
	}
	assert.ElementsMatch(t, []string{"sink.go:2", "sink.go:3", "consumer.go:3", "sink_test.go:5", "external_test.go:3", "helper_test.go:2"}, got)
}

func TestChannelDirectionScopes(t *testing.T) {
	t.Parallel()
	const source = "package p\nfunc Send(c chan int) { c <- 1 }\n"
	offenses := coptest.RunProgram(t, ChannelDirection, coptest.ProgramFiles{
		"pkg/config/v0/sample.go":     source,
		"pkg/config/latest/sample.go": source,
		"generated/sample.go":         "// Code generated by test; DO NOT EDIT.\n" + source,
		"caller/sample.go": `package p
import old "example.test/pkg/config/v0"
func f(c chan int) { old.Send(c) }
`,
	})
	assert.Len(t, offenses, 1)
	if len(offenses) == 1 {
		assert.Contains(t, filepath.ToSlash(offenses[0].Pos.Filename), "/latest/")
	}
}

func TestChannelDirectionBridge(t *testing.T) {
	t.Parallel()
	offenses := coptest.RunProgram(t, ChannelDirection, coptest.ProgramFiles{
		"bridge.go": `package p
type bridge struct { ch chan int }
func (b *bridge) swap(ch chan int) chan int { prev := b.ch; b.ch = ch; return prev }
func (b *bridge) restore(current, previous chan int) { b.ch = previous; close(current) }
func (b *bridge) send() { b.ch <- 1 }
func (b *bridge) run(events chan int) { prev := b.swap(events); b.restore(events, prev) }
`,
	})
	assert.Len(t, offenses, 5)
	for _, offense := range offenses {
		assert.Contains(t, offense.Message, "send-only")
	}
}

func TestChannelDirectionInvalidTests(t *testing.T) {
	t.Parallel()
	assert.Empty(t, coptest.RunProgram(t, ChannelDirection, coptest.ProgramFiles{
		"sample.go":      "package p\nfunc f(c chan int) { <-c }",
		"sample_test.go": "package p\nfunc broken() { close() }\nfunc bodyless(c chan int)",
	}))
}

func TestChannelDirectionRepeatedInterfaceUses(t *testing.T) {
	t.Parallel()
	offenses := coptest.RunProgram(t, ChannelDirection, coptest.ProgramFiles{
		"sample.go": `package p
type Sender interface { Send(chan int) }
type Receiver interface { Send(<-chan int) }
type Worker struct{}
func (Worker) Send(c chan int) { c <- 1 }
func use(a, b Sender) { _ = a; _ = b; _ = []Sender{a,b} }
func anonymous(a, b interface{ Send(chan int) }) { _ = a; _ = b }
func narrow(c chan int) { c <- 1 }
`,
	})
	assert.Len(t, offenses, 1)
	if len(offenses) == 1 {
		assert.Equal(t, 8, offenses[0].Pos.Line)
	}
}
