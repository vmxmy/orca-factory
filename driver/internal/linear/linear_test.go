package linear

import (
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "net/http"
 "net/http/httptest"
 "reflect"
 "strings"
 "testing"
 "time"

 "github.com/vmxmy/orca-factory/driver/internal/infra"
)

type request struct { Query string; Variables map[string]any }
func fixture(t *testing.T, handle func(http.ResponseWriter, request)) (*Client, *int) {
 t.Helper()
 t.Setenv("LINEAR_API_KEY", "test-private-key")
 calls := new(int)
 server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
  *calls++
  if r.Header.Get("Authorization") != "test-private-key" { t.Error("authorization missing") }
  if r.Method != "POST" { t.Error("expected POST") }
  var q request
  if err:=json.NewDecoder(r.Body).Decode(&q); err!=nil { t.Error(err) }
  w.Header().Set("X-Complexity","7")
  w.Header().Set("X-RateLimit-Requests-Limit","2500")
  w.Header().Set("X-RateLimit-Requests-Remaining","2490")
  w.Header().Set("X-RateLimit-Requests-Reset","1800000000000")
  w.Header().Set("X-RateLimit-Complexity-Limit","3000000")
  w.Header().Set("X-RateLimit-Complexity-Remaining","2999990")
  w.Header().Set("X-RateLimit-Complexity-Reset","1800000000001")
  if strings.Contains(q.Query,"Metadata") {
   if q.Variables["team"]!="team-id" { t.Error("missing team scope") }
   field:="states"; nodes:=`[{"id":"s1","name":"Done"}]`
   if strings.Contains(q.Query,"labels(") { field="labels"; nodes=`[{"id":"l1","name":"bug"},{"id":"l2","name":"old"}]` }
   if strings.Contains(q.Query,"projects(") { field="projects"; nodes=`[{"id":"p1","name":"Alpha"},{"id":"p2","name":"Beta"}]` }
   fmt.Fprintf(w,`{"data":{"team":{"%s":{"nodes":%s,"pageInfo":{"hasNextPage":false}}}}}`,field,nodes)
   return
  }
  handle(w,q)
 }))
 t.Cleanup(server.Close)
 c,err:=New(context.Background(),Options{TeamID:"team-id",BaseURL:server.URL,HTTPClient:server.Client()})
 if err!=nil { t.Fatal(err) }
 return c,calls
}
func TestMissingKey(t *testing.T) {
 t.Setenv("LINEAR_API_KEY","")
 _,err:=New(context.Background(),Options{TeamID:"team-id"})
 if err==nil || !strings.Contains(err.Error(),"LINEAR_API_KEY") { t.Fatalf("missing key error: %v",err) }
}
func TestSnapshot(t *testing.T) {
 for _,since:=range []time.Time{{},time.Date(2026,1,2,3,4,5,0,time.UTC)} {
  t.Run(fmt.Sprint(since.IsZero()),func(t *testing.T){
   pages:=0
   c,calls:=fixture(t,func(w http.ResponseWriter,q request){
    pages++
    filter:=q.Variables["filter"].(map[string]any)
    if !reflect.DeepEqual(filter["project"],map[string]any{"id":map[string]any{"in":[]any{"p1","p2"}}}) { t.Errorf("project filter: %v",filter) }
    if since.IsZero() { if _,ok:=filter["updatedAt"];ok {t.Error("zero time filtered")} } else if !reflect.DeepEqual(filter["updatedAt"],map[string]any{"gt":"2026-01-02T03:04:05Z"}) {t.Errorf("time filter: %v",filter)}
    if pages==1 { fmt.Fprint(w,`{"data":{"issues":{"nodes":[{"id":"i1","identifier":"ENG-1","updatedAt":"2026-01-03T00:00:00Z","state":{"id":"s1","name":"Done"},"labels":{"nodes":[{"id":"l1","name":"bug"}]},"relations":{"nodes":[{"id":"r1","type":"blocks","relatedIssue":{"id":"i2","identifier":"ENG-2"}}]}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`) } else {
     if q.Variables["after"]!="next" {t.Error("missing cursor")}
     fmt.Fprint(w,`{"data":{"issues":{"nodes":[{"id":"i2","identifier":"ENG-2"}],"pageInfo":{"hasNextPage":false}}}}`)
    }
   })
   issues,err:=c.Snapshot(context.Background(),[]string{"Alpha","Beta"},since)
   if err!=nil {t.Fatal(err)}
   if len(issues)!=2 || issues[0].Identifier!="ENG-1" || issues[0].State.Name!="Done" || issues[0].Labels.Nodes[0].Name!="bug" || issues[0].Relations.Nodes[0].RelatedIssue.Identifier!="ENG-2" || issues[0].UpdatedAt.Format(time.RFC3339)!="2026-01-03T00:00:00Z" {t.Fatalf("issues: %+v",issues)}
   if *calls!=5 {t.Errorf("calls=%d",*calls)}
  })
 }
}
func TestUpdateAndUsage(t *testing.T) {
 c,calls:=fixture(t,func(w http.ResponseWriter,q request){
  want:=map[string]any{"stateId":"s1","addedLabelIds":[]any{"l1"},"removedLabelIds":[]any{"l2"}}
  if !strings.Contains(q.Query,"issueUpdate") || q.Variables["id"]!="i1" || !reflect.DeepEqual(q.Variables["input"],want) {t.Errorf("mutation: %+v",q)}
  fmt.Fprint(w,`{"data":{"issueUpdate":{"success":true,"issue":{"id":"i1","identifier":"ENG-1","state":{"id":"s1","name":"Done"}}}}}`)
 })
 issue,err:=c.UpdateIssue(context.Background(),"i1","Done",[]string{"bug"},[]string{"old"})
 if err!=nil || issue.Identifier!="ENG-1" {t.Fatalf("update: %+v %v",issue,err)}
 if *calls!=4 {t.Errorf("update must make one request, total=%d",*calls)}
 u:=c.Usage()
 if u.Requests!=4 || u.Complexity!=28 || u.RequestLimit.Limit!=2500 || u.RequestLimit.Remaining!=2490 || u.RequestLimit.Reset.UnixMilli()!=1800000000000 || u.ComplexityLimit.Limit!=3000000 || u.ComplexityLimit.Remaining!=2999990 || u.ComplexityLimit.Reset.UnixMilli()!=1800000000001 {t.Fatalf("usage: %+v",u)}
 _,err=c.UpdateIssue(context.Background(),"i1","unknown",nil,nil)
 if err==nil || *calls!=4 {t.Error("unknown name must fail locally")}
}
func TestComments(t *testing.T) {
 pages:=0
 c,calls:=fixture(t,func(w http.ResponseWriter,q request){
  if strings.Contains(q.Query,"commentCreate") {
   if !reflect.DeepEqual(q.Variables["input"],map[string]any{"issueId":"i1","body":"hello"}) {t.Error("comment input")}
   fmt.Fprint(w,`{"data":{"commentCreate":{"success":true,"comment":{"id":"c1","body":"hello"}}}}`);return
  }
  pages++
  if q.Variables["id"]!="i1" {t.Error("issue id")}
  if pages==1 {fmt.Fprint(w,`{"data":{"issue":{"comments":{"nodes":[{"id":"c1","body":"hello"}],"pageInfo":{"hasNextPage":true,"endCursor":"c-next"}}}}}`)} else {
   if q.Variables["after"]!="c-next" {t.Error("comments cursor")}
   fmt.Fprint(w,`{"data":{"issue":{"comments":{"nodes":[{"id":"c2","body":"world"}],"pageInfo":{"hasNextPage":false}}}}}`)
  }
 })
 comment,err:=c.CreateComment(context.Background(),"i1","hello")
 if err!=nil || comment.ID!="c1" {t.Fatalf("create: %+v %v",comment,err)}
 comments,err:=c.ListComments(context.Background(),"i1")
 if err!=nil || len(comments)!=2 || comments[1].Body!="world" || *calls!=6 {t.Fatalf("list: %+v %v",comments,err)}
}
func TestErrorsAreSanitized(t *testing.T) {
 for _,tc:=range []struct{status int; body string; global bool; transient bool}{
  {400,`{"errors":[{"message":"test-private-key","extensions":{"code":"RATELIMITED"}}]}`,true,false},
  {200,`{"errors":[{"message":"test-private-key"}]}`,false,false},
  {500,`test-private-key`,false,true},
  {200,`test-private-key`,false,false},
  {200,`{"data":{"issueUpdate":{"success":false}}}`,false,false},
 } {t.Run(fmt.Sprint(tc.status,tc.global,tc.transient,tc.body[:1]),func(t *testing.T){
  c,_:=fixture(t,func(w http.ResponseWriter,q request){w.WriteHeader(tc.status);fmt.Fprint(w,tc.body)})
  _,err:=c.UpdateIssue(context.Background(),"i1","Done",nil,nil)
  if err==nil {t.Fatal("expected error")}
  for e:=err;e!=nil;e=errors.Unwrap(e) {if strings.Contains(e.Error(),"test-private-key") {t.Fatal("credential leaked")}}
  var g *infra.Global; if errors.As(err,&g)!=tc.global || (g!=nil && g.Dep!=infra.Linear) {t.Fatalf("classification: %T",err)}
  var tr *infra.Transient; if errors.As(err,&tr)!=tc.transient {t.Fatalf("classification: %T",err)}
 })}
}
func TestRedirectDoesNotForwardKey(t *testing.T) {
 reached:=false
 target:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){reached=true}))
 defer target.Close()
 c,_:=fixture(t,func(w http.ResponseWriter,q request){w.Header().Set("Location",target.URL);w.WriteHeader(307)})
 _,err:=c.CreateComment(context.Background(),"i1","hello")
 if err==nil || reached {t.Fatal("redirect followed")}
}
