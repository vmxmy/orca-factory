package executor

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

 "github.com/vmxmy/orca-factory/driver/internal/github"
 "github.com/vmxmy/orca-factory/driver/internal/linear"
 "github.com/vmxmy/orca-factory/driver/internal/machine"
 "github.com/vmxmy/orca-factory/driver/internal/orca"
 "github.com/vmxmy/orca-factory/driver/internal/runx"
)

type recoveryOrca struct { fakeOrca; starts []orca.StartOptions; failFirst bool }
func (o *recoveryOrca) WorkerStart(ctx context.Context, id string, opts orca.StartOptions) (orca.Receipt,error) {
 o.starts=append(o.starts,opts)
 r,err:=o.fakeOrca.WorkerStart(ctx,id,opts)
 r["worktreePath"]=json.RawMessage(`"/original/worktree"`)
 return r,err
}
func (o *recoveryOrca) TaskList(context.Context,string)([]orca.Task,error){
 outcome:="succeeded"; if o.failFirst && len(o.starts)==1 {outcome="failed"}
 return []orca.Task{{ID:"task_one",RunID:"run_one",DispatchID:"ctx_one",Result:json.RawMessage(`{"provenance":"worker_report","outcome":"`+outcome+`"}`)}},nil
}
type recoveryTools struct { fakeTools; gateReds,reviewReds,emptyReviews int; gateErr error }
func (f *recoveryTools) Gate(ctx context.Context,repo,head,spec string)(string,error){
 if f.gateErr!=nil { return "",f.gateErr }
 if f.gateReds>0 {f.gateReds--;f.gateCalls++;return "VERDICT red\n",nil}
 return f.fakeTools.Gate(ctx,repo,head,spec)
}
func (f *recoveryTools) Review(ctx context.Context,repo,base,head string)(string,error){
 if f.emptyReviews>0 {f.emptyReviews--;f.reviewCalls++;return "",nil}
 if f.reviewReds>0 {f.reviewReds--;f.reviewCalls++;return `{"verdict":"red","step":"review"}`,nil}
 return f.fakeTools.Review(ctx,repo,base,head)
}
type recoveryService struct { sessions,children int; missing bool; complete bool; requests []CorrectionRequest }
func (r *recoveryService) Correction(_ context.Context, req CorrectionRequest) error {
 r.sessions++;r.requests=append(r.requests,req)
 if r.missing {return nil}; return os.WriteFile(req.Path,[]byte("Repair the failing assertion"),0600)
}
func (r *recoveryService) ChildTask(_ context.Context,run,parent,spec,request string)(string,error){
 if run!="run_one" || parent!="task_one" || !strings.Contains(spec,"Repair the failing assertion") {return "",errors.New("bad child task")}
 r.children++;return "child_task",nil
}
func (r *recoveryService) Complete(context.Context,string,string)(bool,error){return r.complete,nil}
func (r *recoveryService) Worktree(context.Context,string,string)(string,error){return "/original/worktree",nil}

type budgetLinear struct {fakeLinear; updates []struct{state string;add,remove []string}}
func (l *budgetLinear) UpdateIssue(ctx context.Context,id,state string,add,remove []string)(linear.Issue,error){
 l.updates=append(l.updates,struct{state string;add,remove []string}{state,add,remove})
 return l.fakeLinear.UpdateIssue(ctx,id,state,add,remove)
}
func recoveryFixture(t *testing.T)(*Executor,*recoveryOrca,*recoveryTools,*recoveryService,*budgetLinear,Ticket){
 e,_,_,_,_,ticket:=fixture(t,"new-sha")
 o:=&recoveryOrca{}; tools:=&recoveryTools{fakeTools:fakeTools{head:"new-sha"}}; service:=&recoveryService{}; l:=&budgetLinear{}
 e.Orca=o;e.Tools=tools;e.Recovery=service;e.Linear=l;e.Sleep=func(context.Context,time.Duration)error{return nil}
 return e,o,tools,service,l,ticket
}
func TestCorrectionLoop(t *testing.T){
 for _,cause:=range []string{"gate","review","worker"}{t.Run(cause,func(t *testing.T){
 e,o,tools,s,_,ticket:=recoveryFixture(t)
 switch cause{case "gate":tools.gateReds=1;case "review":tools.reviewReds=1;case "worker":o.failFirst=true}
 result,err:=e.Execute(context.Background(),ticket)
 if err!=nil||result.State.Owner!=machine.OwnerMerge||s.sessions!=1||s.children!=1||len(o.starts)!=2 {t.Fatalf("result=%+v err=%v sessions=%d children=%d starts=%d",result,err,s.sessions,s.children,len(o.starts))}
 child:=o.starts[1];if child.Worktree!="path:/original/worktree"||child.Task!="child_task"||child.Spec!=""||child.Repo!=""||child.BaseBranch!=""||child.Name!="" {t.Fatalf("child: %+v",child)}
 })}
}
func TestCorrectionTierAndCap(t *testing.T){
 for _,escalate:=range []string{"0","1"}{t.Run(escalate,func(t *testing.T){
 t.Setenv("AGENT_TIER_ESCALATE",escalate);t.Setenv("AGENT_TIER_FAST","codex:fast-model:low");t.Setenv("AGENT_TIER_DEFAULT","codex:default-model:medium");t.Setenv("AGENT_TIER_STRONG","claude:strong-model:high")
 e,o,tools,s,_,ticket:=recoveryFixture(t);e.Config.Tier="fast";e.Config.Model="fast-model";e.Config.Effort="low";tools.gateReds=5
 result,err:=e.Execute(context.Background(),ticket)
 if err!=nil||result.State.Owner!=machine.OwnerHuman||s.sessions!=3||len(o.starts)!=4 {t.Fatalf("result=%+v err=%v sessions=%d starts=%d",result,err,s.sessions,len(o.starts))}
 want:=[]string{"fast-model","fast-model","fast-model","fast-model"};if escalate=="1" {want=[]string{"fast-model","default-model","strong-model","strong-model"}}
 for i,start:=range o.starts {if start.Model!=want[i]{t.Fatalf("start %d model=%s want=%s",i,start.Model,want[i])}}
 })}
}
func TestCorrectionMissingArtifactBudget(t *testing.T){
 e,o,tools,s,l,ticket:=recoveryFixture(t);tools.gateReds=1;s.missing=true
 result,err:=e.Execute(context.Background(),ticket)
 if err!=nil||result.State.Infra!=3||result.State.Owner!=machine.OwnerHuman||s.sessions!=3||len(o.starts)!=1||s.children!=0 {t.Fatalf("result=%+v err=%v sessions=%d children=%d starts=%d",result,err,s.sessions,s.children,len(o.starts))}
 assertBudgetHandoff(t,l)
}
func TestInfraBudgetBackoff(t *testing.T){
 e,_,tools,_,_,ticket:=recoveryFixture(t);tools.emptyReviews=2
 var sleeps []time.Duration;e.Sleep=func(_ context.Context,d time.Duration)error{sleeps=append(sleeps,d);return nil}
 result,err:=e.Execute(context.Background(),ticket)
 if err!=nil||result.State.Owner!=machine.OwnerMerge||result.State.Infra!=2||!reflect.DeepEqual(sleeps,[]time.Duration{time.Minute,5*time.Minute}) {t.Fatalf("result=%+v err=%v sleeps=%v",result,err,sleeps)}
}
func TestInfraBudgetExhaustion(t *testing.T){
 e,_,tools,_,l,ticket:=recoveryFixture(t);tools.emptyReviews=3
 result,err:=e.Execute(context.Background(),ticket)
 if err!=nil||result.State.Infra!=3||result.State.Owner!=machine.OwnerHuman {t.Fatalf("result=%+v err=%v",result,err)}
 assertBudgetHandoff(t,l)
}
func assertBudgetHandoff(t *testing.T,l *budgetLinear){
 t.Helper();last:=l.updates[len(l.updates)-1]
 if last.state!="In Progress"||!reflect.DeepEqual(last.add,[]string{"auto-impl","auto-failed"})||!reflect.DeepEqual(last.remove,[]string{"ready-for-agent"}) {t.Fatalf("labels=%+v",last)}
 lines:=strings.Split(l.comments[len(l.comments)-1],"\n")
 if len(lines)!=3||!strings.HasPrefix(lines[0],"转人: ")||!strings.HasPrefix(lines[1],"证据: ")||!strings.HasPrefix(lines[2],"需要你: "){t.Fatalf("handoff=%q",lines)}
}
func TestResumeTaskResult(t *testing.T){
 e,o,tools,_,_,ticket:=recoveryFixture(t)
 state:=checkpoint{Run:"run_one",Task:"task_one",Dispatch:"ctx_one",Worktree:"/original/worktree",Baseline:"base-sha"}
 if err:=saveCheckpoint(e.Config,ticket,state);err!=nil{t.Fatal(err)}
 result,err:=e.Start(context.Background(),ticket)
 if err!=nil||result.State.Owner!=machine.OwnerMerge||len(o.starts)!=0||tools.gateCalls!=1 {t.Fatalf("result=%+v err=%v starts=%d gates=%d",result,err,len(o.starts),tools.gateCalls)}
}
func TestResumeCompleteBranch(t *testing.T){
 e,o,tools,s,_,ticket:=recoveryFixture(t);s.complete=true
 result,err:=e.Start(context.Background(),ticket)
 if err!=nil||result.State.Owner!=machine.OwnerMerge||len(o.starts)!=0||tools.gateCalls!=1 {t.Fatalf("result=%+v err=%v starts=%d gates=%d",result,err,len(o.starts),tools.gateCalls)}
}
func TestResumeExistingPR(t *testing.T){
 e,o,tools,_,_,ticket:=recoveryFixture(t)
 e.GitHub.(*fakeGitHub).existing=&github.PullRequest{Number:7,HTMLURL:"https://example.test/pr/7"}
 result,err:=e.Start(context.Background(),ticket)
 if err!=nil||result.State.Owner!=machine.OwnerMerge||result.PR.Number!=7||len(o.starts)!=0||tools.gateCalls!=0||tools.reviewCalls!=0 {t.Fatalf("result=%+v err=%v starts=%d gate=%d review=%d",result,err,len(o.starts),tools.gateCalls,tools.reviewCalls)}
}
func TestCorrectionCommands(t *testing.T){
 root:=t.TempDir();target:=filepath.Join(root,"correction.md")
 c:=Commands{Run:func(_ context.Context,s runx.Spec)([]byte,error){
 if s.Path!="claude"||len(s.Args)!=2||s.Args[0]!="-p"||!strings.Contains(s.Args[1],target)||s.Timeout<=0 {t.Fatalf("session=%+v",s)}
 return nil,nil
 }}
 if err:=c.Correction(context.Background(),CorrectionRequest{Repo:root,Path:target,Cause:"gate red"});err!=nil{t.Fatal(err)}
}
