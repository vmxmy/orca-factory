package ledger

import (
 "bytes"
 "context"
 "errors"
 "strings"
 "sync"
 "testing"
 "time"
)

type fakeStore struct {
 mu sync.Mutex
 issues []Issue
 comments map[string][]Comment
 created []Issue
 states []string
 writes []string
 fail bool
 notified chan struct{}
}
func (f *fakeStore) ListIssues(context.Context) ([]Issue,error) { return f.issues,nil }
func (f *fakeStore) ListComments(_ context.Context,id string) ([]Comment,error) { return f.comments[id],nil }
func (f *fakeStore) CreateIssue(_ context.Context,title,state string) (Issue,error) {
 i:=Issue{ID:"new",Title:title,State:state}; f.created=append(f.created,i); f.issues=append(f.issues,i); return i,nil
}
func (f *fakeStore) SetState(_ context.Context,id,state string) error { f.states=append(f.states,id+":"+state); return nil }
func (f *fakeStore) CreateComment(_ context.Context,id,body string) error {
 f.mu.Lock(); defer f.mu.Unlock()
 if f.fail { return errors.New("write failed") }
 f.writes=append(f.writes,id+":"+body)
 if f.notified!=nil { f.notified<-struct{}{} }; return nil
}
var now = time.Date(2026,10,5,12,0,0,0,time.UTC)
func newFake() *fakeStore { return &fakeStore{issues:[]Issue{{ID:"old",Title:"[ledger] agent-graph",UpdatedAt:now}},comments:map[string][]Comment{}} }
func openTest(f *fakeStore) *Ledger { return New(f,Options{Now:func() time.Time{return now},Ticks:make(chan time.Time)}) }
func TestBatchFlushAndClose(t *testing.T) {
 f:=newFake(); l:=openTest(f); ctx:=context.Background()
 for _,row:=range []string{`{ "ticket": "A", "started_at": "one" }`,`{"ticket":"B","started_at":"two"}`} { if err:=l.Append(row);err!=nil {t.Fatal(err)} }
 if len(f.writes)!=0 {t.Fatal("Append wrote before flush")}
 if err:=l.Flush(ctx);err!=nil {t.Fatal(err)}
 want:="old:ledger {\"ticket\":\"A\",\"started_at\":\"one\"}\nledger {\"ticket\":\"B\",\"started_at\":\"two\"}"
 if len(f.writes)!=1 || f.writes[0]!=want {t.Fatalf("writes=%q",f.writes)}
 if err:=l.Close(ctx);err!=nil {t.Fatal(err)}
 if len(f.writes)!=1 {t.Fatal("empty flush wrote comment")}
 if err:=l.Append(`{}`);err==nil {t.Fatal("append after close")}
}
func TestTimerAndFailedFlushRetainsBuffer(t *testing.T) {
 f:=newFake(); f.notified=make(chan struct{},1); ticks:=make(chan time.Time)
 l:=New(f,Options{Now:func()time.Time{return now},Ticks:ticks}); ctx:=context.Background()
 if err:=l.Append(`{"ticket":"A"}`);err!=nil {t.Fatal(err)}
 f.fail=true
 if err:=l.Flush(ctx);err==nil {t.Fatal("write failure lost")}
 f.fail=false; ticks<-now.Add(10*time.Minute)
 select {case <-f.notified: case <-time.After(time.Second):t.Fatal("timer did not flush")}
 if err:=l.Close(ctx);err!=nil {t.Fatal(err)}
 if len(f.writes)!=1 || f.writes[0]!=`old:ledger {"ticket":"A"}` {t.Fatalf("writes=%q",f.writes)}
}
func TestRotation(t *testing.T) {
 for _,tc:=range []struct{title,want string;count int}{{"[ledger] agent-graph","[ledger] agent-graph #2",400},{"[ledger] agent-graph #9","[ledger] agent-graph #10",401},{"[ledger] agent-graph","",399}} {
 t.Run(tc.title+tc.want,func(t *testing.T){
 f:=newFake();f.issues[0].Title=tc.title;f.comments["old"]=make([]Comment,tc.count)
 l:=openTest(f);if err:=l.Append(`{}`);err!=nil {t.Fatal(err)};if err:=l.Close(context.Background());err!=nil {t.Fatal(err)}
 if tc.want=="" {if len(f.created)!=0 {t.Fatal("early rotation")};return}
 if len(f.created)!=1 || f.created[0].Title!=tc.want || f.created[0].State!="Backlog" {t.Fatalf("created=%+v",f.created)}
 if len(f.states)!=1 || f.states[0]!="old:Done" || f.writes[0]!="new:ledger {}" {t.Fatalf("states=%q writes=%q",f.states,f.writes)}
 }) }
}
func TestStatsFormatDedupAndInvalidLines(t *testing.T) {
 f:=newFake(); f.comments["old"]=[]Comment{{Body:`ledger {"ticket":"A","started_at":"1","lane":2,"tier":"fast","outcome":"merged","first_gate":"green","corrections":1,"ci_reds":2,"minutes":1.235}
ledger {"ticket":"B","started_at":"2","lane":2,"outcome":"escalated","first_gate":"red","corrections":0,"minutes":2}
ledger {"started_at":"3","outcome":"no_ticket","lane":1}
ledger {"ticket":"A","started_at":"1","lane":2,"tier":"fast","outcome":"merged","first_gate":"green","corrections":1,"ci_reds":2,"minutes":1.235}
ordinary comment
ledger bad
ledger []
ledger {"corrections":"bad"}`}}
 l:=openTest(f);defer l.Close(context.Background());var out,warnings bytes.Buffer
 if err:=l.Stats(context.Background(),false,&out,&warnings);err!=nil {t.Fatal(err)}
 want:="runs=3 merged=1 escalated=1 no_ticket=1\navg_corrections=0.5 ci_reds=2 avg_minutes=1.62\nlane=1 runs=1 first_gate_green_pct=0\nlane=2 runs=2 first_gate_green_pct=50\ntier=fast runs=1 first_gate_green_pct=100\ntier=unknown runs=1 first_gate_green_pct=0\n"
 if out.String()!=want {t.Fatalf("got:\n%swant:\n%s",out.String(),want)}
 if strings.Count(warnings.String(),"invalid ledger comment skipped")!=3 {t.Fatalf("warnings=%q",warnings.String())}
}
func TestStatsWindow(t *testing.T) {
 f:=newFake();old:=now.Add(-8*24*time.Hour)
 f.issues=[]Issue{{ID:"old",Title:"[ledger] agent-graph",UpdatedAt:old},{ID:"new",Title:"[ledger] agent-graph #2",UpdatedAt:old},{ID:"ignore",Title:"[ledger] agent-graph unrelated",UpdatedAt:now}}
 f.comments["old"]=[]Comment{{Body:`ledger {"ticket":"old"}`,CreatedAt:old}}
 f.comments["new"]=[]Comment{{Body:`ledger {"ticket":"recent"}`,CreatedAt:now.Add(-7*24*time.Hour)}}
 f.comments["ignore"]=[]Comment{{Body:`ledger {"ticket":"ignored"}`,CreatedAt:now}}
 l:=openTest(f);defer l.Close(context.Background())
 for _,all:=range []bool{false,true} {var out bytes.Buffer;if err:=l.Stats(context.Background(),all,&out,&out);err!=nil {t.Fatal(err)}
 want:="runs=1 merged=0 escalated=0 no_ticket=0\navg_corrections=0 ci_reds=0 avg_minutes=0\ntier=unknown runs=1 first_gate_green_pct=0\n"
 if all {want="runs=2 merged=0 escalated=0 no_ticket=0\navg_corrections=0 ci_reds=0 avg_minutes=0\ntier=unknown runs=2 first_gate_green_pct=0\n"}
 if out.String()!=want {t.Fatalf("all=%v: %q",all,out.String())}
 }
}
func TestEmptyAndInvalidAppend(t *testing.T) {
 f:=newFake();l:=openTest(f);defer l.Close(context.Background());var out bytes.Buffer
 if err:=l.Stats(context.Background(),false,&out,&out);err!=nil {t.Fatal(err)}
 if out.String()!="runs=0\n" {t.Fatalf("got %q",out.String())}
 for _,row:=range []string{"", "null","[]","{} {}",`{"minutes":"x"}`} {if l.Append(row)==nil {t.Fatalf("accepted %q",row)}}
}
