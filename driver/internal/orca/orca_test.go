package orca

import (
 "context"
 "encoding/json"
 "errors"
 "os"
 "path/filepath"
 "reflect"
 "strings"
 "testing"
 "time"

 "github.com/vmxmy/orca-factory/driver/internal/infra"
)

const firstID = "5e1dde31-cd0d-59a8-8358-5caf59d9e87f"
const secondID = "4a07ab07-4616-5d22-80ea-f8d9c5109195"

func fake(t *testing.T) (*Client, string) {
 t.Helper()
 path, err := filepath.Abs("testdata/orca"); if err != nil { t.Fatal(err) }
 log := filepath.Join(t.TempDir(), "argv")
 t.Setenv("AGENT_ORCA_BIN", path); t.Setenv("ORCA_TEST_LOG", log); t.Setenv("ORCA_TEST_SCENARIO", "")
 return &Client{Timeout: time.Second}, log
}
func argv(t *testing.T, log string, want []string) {
 t.Helper(); b, err := os.ReadFile(log); if err != nil { t.Fatal(err) }
 got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
 if !reflect.DeepEqual(got,want) { t.Fatalf("argv = %q, want %q",got,want) }
}
func TestRequestID(t *testing.T) {
 for i := 0; i < 2; i++ { if got := RequestID("ZII-858","impl","start",0); got != firstID { t.Fatalf("UUID %s", got) } }
 if got := RequestID("ZII-858","impl","start",1); got != secondID { t.Fatalf("UUID %s", got) }
}
func TestCommands(t *testing.T) {
 ctx := context.Background()
 cases := []struct{name string; call func(*Client)(any,error); args []string; want string}{
  {"run",func(c *Client)(any,error){return c.RunCreate(ctx,firstID,"build it")},[]string{"orchestration","run-create","--objective","build it","--retry-request",firstID,"--json"},`{"run":{"id":"run_one","objective":"build it"}}`},
  {"start",func(c *Client)(any,error){return c.WorkerStart(ctx,firstID,StartOptions{Run:"run_one",Spec:"do work",Worktree:"new-top-level",Agent:"codex",Model:"m",Effort:"high",Setup:"skip",Name:"impl",Repo:"path:/repo",BaseBranch:"origin/main"})},[]string{"orchestration","worker-start","--run","run_one","--spec","do work","--worktree","new-top-level","--agent","codex","--model","m","--effort","high","--setup","skip","--name","impl","--repo","path:/repo","--base-branch","origin/main","--retry-request",firstID,"--json"},`{"taskId":"task_one","dispatchId":"ctx_one","status":"ready","terminalHandle":"term_one"}`},
  {"read",func(c *Client)(any,error){return c.WorkerRead(ctx,ReadOptions{Dispatch:"ctx_one",Source:"transcript",Cursor:"old",Limit:5})},[]string{"orchestration","worker-read","--dispatch","ctx_one","--source","transcript","--cursor","old","--limit","5","--json"},`{"dispatchId":"ctx_one","source":"transcript","cursor":"next","transcript":{"messages":[{"role":"assistant"}]}}`},
  {"list",func(c *Client)(any,error){return c.WorkerList(ctx,ListOptions{Run:"run_one",TerminalState:"reclaimable",Cursor:"old",Limit:5,IncludeRemote:true})},[]string{"orchestration","worker-list","--run","run_one","--terminal-state","reclaimable","--cursor","old","--limit","5","--include-remote","--json"},`{"workers":[{"dispatchId":"ctx_one","taskId":"task_one","terminalState":"reclaimable"}],"page":{"hasMore":true,"nextCursor":"page2"}}`},
  {"release",func(c *Client)(any,error){return c.WorkerRelease(ctx,firstID,"ctx_one")},[]string{"orchestration","worker-release","--dispatch","ctx_one","--retry-request",firstID,"--json"},`{"dispatchId":"ctx_one","status":"released"}`},
  {"terminal-create",func(c *Client)(any,error){return c.TerminalCreate(ctx,TerminalOptions{Worktree:"current",Title:"runner",Command:"codex"})},[]string{"terminal","create","--worktree","current","--title","runner","--command","codex","--json"},`{"terminal":{"handle":"term_one"}}`},
  {"terminal-show",func(c *Client)(any,error){return c.TerminalShow(ctx,"term_one")},[]string{"terminal","show","--terminal","term_one","--json"},`{"terminal":{"handle":"term_one","preview":"hello"}}`},
  {"automations",func(c *Client)(any,error){return c.AutomationsList(ctx)},[]string{"automations","list","--json"},`{"automations":[{"id":"auto_one","name":"nightly","enabled":true}]}`},
 }
 for _,tc := range cases {t.Run(tc.name,func(t *testing.T){
  c,log:=fake(t); got,err:=tc.call(c); if err!=nil {t.Fatal(err)}
  b,err:=json.Marshal(got);if err!=nil{t.Fatal(err)};var actual,want any
  if err=json.Unmarshal(b,&actual);err!=nil{t.Fatal(err)};if err=json.Unmarshal([]byte(tc.want),&want);err!=nil{t.Fatal(err)}
  if !reflect.DeepEqual(actual,want){t.Fatalf("result %s, want %s",b,tc.want)}
  argv(t,log,append(tc.args,"END"))
 })}
}
func TestTaskListWorkerReport(t *testing.T){
 c,log:=fake(t); tasks,err:=c.TaskList(context.Background(),"run_one");if err!=nil{t.Fatal(err)}
 if len(tasks)!=1 || tasks[0].ID!="task_one" || tasks[0].RunID!="run_one" || tasks[0].DispatchID!="ctx_one" || tasks[0].Status!="completed" {t.Fatalf("tasks %#v",tasks)}
 report,err:=tasks[0].WorkerReport();if err!=nil{t.Fatal(err)}
 want:=&WorkerReport{Provenance:"worker_report",Outcome:"succeeded",Body:"done",FilesModified:[]string{"a.go"}}
 if !reflect.DeepEqual(report,want){t.Fatalf("report %#v",report)}
 argv(t,log,[]string{"orchestration","task-list","--run","run_one","--json","END"})
}
func TestReconcile(t *testing.T){
 for _,state:=range []string{"completed","pending","absent"}{t.Run(state,func(t *testing.T){
  c,log:=fake(t);t.Setenv("ORCA_TEST_SCENARIO",state)
  got,err:=c.Reconcile(context.Background(),"request_one","run_one");if err!=nil{t.Fatal(err)}
  if got.State!=state{t.Fatalf("state %s",got.State)}
  want:=[]string{"orchestration","request-show","--request","request_one","--json","END"}
  if state=="completed"{if string(got.Receipt)!=`{"taskId":"task_one","dispatchId":"ctx_one","status":"ready"}` || len(got.Tasks)!=0{t.Fatalf("receipt %s",got.Receipt)}}else{
   if len(got.Tasks)!=1 || got.Tasks[0].ID!="task_one" || len(got.Receipt)!=0{t.Fatalf("reconciliation %#v",got)}
   want=append(want,"orchestration","task-list","--run","run_one","--json","END")
  };argv(t,log,want)
 })}
}
func TestFailedStartRetry(t *testing.T){
 c,log:=fake(t);ctx:=context.Background();opts:=StartOptions{Task:"task_one",Worktree:"current",Agent:"codex"}
 t.Setenv("ORCA_TEST_SCENARIO","failed");receipt,err:=c.WorkerStart(ctx,firstID,opts)
 var ticket *infra.Ticket
 if !errors.As(err,&ticket) || string(receipt["dispatchId"])!=`"ctx_failed"` || string(receipt["failedStage"])!=`"launch"` {t.Fatalf("failed receipt %#v, %v",receipt,err)}
 t.Setenv("ORCA_TEST_SCENARIO","");opts.RetryOf="ctx_failed"
 _,err=c.WorkerStart(ctx,secondID,opts);if err!=nil{t.Fatal(err)}
 argv(t,log,[]string{"orchestration","worker-start","--task","task_one","--worktree","current","--agent","codex","--retry-request",firstID,"--json","END","orchestration","worker-start","--task","task_one","--worktree","current","--agent","codex","--retry-of","ctx_failed","--retry-request",secondID,"--json","END"})
}
func TestErrorsAndBounds(t *testing.T){
 for _,scenario:=range []string{"malformed","failure","unavailable","timeout"}{t.Run(scenario,func(t *testing.T){
  c,_:=fake(t);t.Setenv("ORCA_TEST_SCENARIO",scenario);c.Timeout=30*time.Millisecond
  _,err:=c.AutomationsList(context.Background());if err==nil{t.Fatal("expected error")}
  if strings.Contains(err.Error(),"private-value"){t.Fatal("unsafe error")}
  var global *infra.Global;var transient *infra.Transient;var ticket *infra.Ticket
  switch scenario {case "unavailable":if !errors.As(err,&global)||global.Dep!=infra.Orca{t.Fatalf("class %T",err)};case "timeout":if !errors.As(err,&transient){t.Fatalf("class %T",err)};default:if !errors.As(err,&ticket){t.Fatalf("class %T",err)}}
 })}
 for _,state:=range []string{"unknown","empty"}{t.Run(state,func(t *testing.T){c,_:=fake(t);t.Setenv("ORCA_TEST_SCENARIO",state);if _,err:=c.Reconcile(context.Background(),"r","run");err==nil{t.Fatal("unsafe reconciliation")}})}
 t.Run("absolute-path",func(t *testing.T){c,log:=fake(t);c.Path="relative";if _,err:=c.AutomationsList(context.Background());err==nil{t.Fatal("relative accepted")};if _,err:=os.Stat(log);!os.IsNotExist(err){t.Fatal("command ran")}})
 t.Run("cancellation",func(t *testing.T){c,_:=fake(t);ctx,cancel:=context.WithCancel(context.Background());cancel();if _,err:=c.AutomationsList(ctx);!errors.Is(err,context.Canceled){t.Fatalf("cancel %v",err)}})
}
func TestWorkerReportShapes(t *testing.T){
 for _,raw:=range []string{`null`,`{"provenance":"coordinator","outcome":"succeeded"}`}{r,err:=(Task{Result:json.RawMessage(raw)}).WorkerReport();if err!=nil||r!=nil{t.Fatalf("non-worker report %#v %v",r,err)}}
 r,err:=(Task{Result:json.RawMessage(`{"provenance":"worker_report","outcome":"failed","body":"reason","filesModified":[]}`)}).WorkerReport();if err!=nil||r.Outcome!="failed"||r.Body!="reason"{t.Fatalf("object report %#v %v",r,err)}
 if _,err:=(Task{Result:json.RawMessage(`"invalid"`)}).WorkerReport();err==nil{t.Fatal("invalid result accepted")}
}
