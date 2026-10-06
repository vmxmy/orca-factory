import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch
import tempfile
import json
import subprocess
import os
from types import SimpleNamespace

spec=importlib.util.spec_from_file_location("supervisor",Path(__file__).with_name("supervisor.py"))
m=importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

class SupervisorTests(unittest.TestCase):
    def test_work_capacity(self):
        args=m.container_args("sha256:"+"a"*64,"name","owner")
        self.assertIn("/home/runner/_work:rw,nosuid,nodev,exec,size=6g,uid=1001,gid=123",args)
    def test_post_failure_retains_intent(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"lease.json"
            def fail(*args,**kwargs):
                self.assertEqual(json.loads(path.read_text())["phase"],"registering")
                raise RuntimeError("response lost")
            with patch.object(m,"api",side_effect=fail):
                with self.assertRaisesRegex(RuntimeError,"response lost"):
                    m.one_cycle(SimpleNamespace(slot=1),"owner",path,"sha256:"+"a"*64)
            self.assertTrue(json.loads(path.read_text())["active"])
    def test_registration_dial_failure_releases_unsent_intent(self):
        url="https://api.github.com/repos/vmxmy/orca-factory/actions/runners/generate-jitconfig"
        for detail in ("198.18.0.76:443: i/o timeout", "127.0.0.1:443: connect: connection refused", "lookup api.github.com: no such host"):
            with self.subTest(detail=detail), tempfile.TemporaryDirectory() as directory:
                path=Path(directory)/"lease.json"
                result=subprocess.CompletedProcess([],1,"",f'Post "{url}": dial tcp {detail}\n')
                with patch.object(m,"command",return_value=result), patch.object(m,"event"), patch.object(m.subprocess,"Popen") as start:
                    with self.assertRaises(RuntimeError):
                        m.one_cycle(SimpleNamespace(slot=4),"owner",path,"sha256:"+"a"*64)
                    start.assert_not_called()
                self.assertFalse(json.loads(path.read_text())["active"])
    def test_ambiguous_registration_errors_keep_intent(self):
        url="https://api.github.com/repos/vmxmy/orca-factory/actions/runners/generate-jitconfig"
        errors=[f'Post "{url}": EOF', f'Post "{url}": read tcp 1:2->3:4: i/o timeout',
                f'Post "{url}": net/http: timeout awaiting response headers',
                f'Post "{url}": proxyconnect tcp: dial tcp 1:443: i/o timeout',
                'gh: HTTP 503: dial tcp backend:443: i/o timeout',
                'Post "https://redirect.example/": dial tcp 1:443: i/o timeout',
                f'Get "{url}": dial tcp 1:443: i/o timeout']
        for error in errors:
            with self.subTest(error=error), tempfile.TemporaryDirectory() as directory:
                path=Path(directory)/"lease.json"
                result=subprocess.CompletedProcess([],1,"",error+"\n")
                with patch.object(m,"command",return_value=result):
                    with self.assertRaises(RuntimeError):
                        m.one_cycle(SimpleNamespace(slot=4),"owner",path,"sha256:"+"a"*64)
                self.assertTrue(json.loads(path.read_text())["active"])
    def test_recover_running_idle_never_stops_container(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"lease.json"
            path.write_text(json.dumps({"active":True,"name":"ours","id":1}))
            def wait(_):m.STOP.set()
            try:
                with patch.object(m,"inspect_container",return_value={"State":{"Running":True}}), patch.object(m,"runner_busy",return_value=False), patch.object(m,"command") as command, patch.object(m.STOP,"wait",side_effect=wait), patch.object(m,"event"):
                    m.recover(path,"owner")
                    command.assert_not_called()
            finally:m.STOP.clear()
    def test_intent_recovery_uses_exact_name(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"lease.json"
            path.write_text(json.dumps({"active":True,"phase":"registering","name":"ours"}))
            def api(method,suffix,**kwargs):
                if suffix.startswith("?per_page"):
                    return {"runners":[{"id":7,"name":"other"},{"id":8,"name":"ours"}]}
                if method=="GET":return {"id":8,"name":"ours","busy":False}
                self.assertEqual((method,suffix),("DELETE","/8"))
            with patch.object(m,"api",side_effect=api),patch.object(m,"inspect_container",return_value=None),patch.object(m,"event"):
                m.recover(path,"owner")
            self.assertFalse(json.loads(path.read_text())["active"])
    def test_missing_container_lowercase(self):
        result=subprocess.CompletedProcess([],1,"[]","error: no such object: ours")
        with patch.object(m,"command",return_value=result):
            self.assertIsNone(m.inspect_container("ours","owner"))
    def test_recover_stop_retains_busy_lease(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/"lease.json"
            path.write_text(json.dumps({"active":True,"name":"ours","id":1}))
            m.STOP.set()
            try:
                with patch.object(m,"inspect_container",side_effect=AssertionError("STOP ignored")) as inspect, patch.object(m,"event"):
                    m.recover(path,"owner")
                    inspect.assert_not_called()
                    self.assertTrue(json.loads(path.read_text())["active"])
            finally:m.STOP.clear()
    def test_container_policy(self):
        args=m.container_args("sha256:"+"a"*64,"orca-go-test-1","owner")
        for pair in [("--user","1001:123"),("--cap-drop","ALL"),("--security-opt","no-new-privileges:true"),("--cpus","4"),("--memory","8g"),("--memory-swap","8g"),("--pids-limit","512"),("--network","bridge")]:
            i=args.index(pair[0]);self.assertEqual(args[i+1],pair[1])
        self.assertFalse(any(x in args for x in ["--privileged","--volume","-v","--mount","--env-file"]))
        self.assertNotIn("GH_TOKEN", " ".join(args))
        self.assertNotIn("jit", " ".join(args).lower())
    def test_image_is_immutable(self):
        with self.assertRaises(ValueError):m.container_args("image:latest","orca-go-test-1","owner")
    def test_jit_request_is_single_repo_labeled(self):
        self.assertEqual(m.jit_payload("name"),{"name":"name","runner_group_id":1,"labels":["self-hosted","Linux","X64","orca-go-isolated"],"work_folder":"_work"})
    def test_limits_enforced(self):
        for n in [0,5,100]:
            with self.assertRaises(ValueError):m.validate_workers(n)
        for n in [1,2,3,4]:m.validate_workers(n)
    def test_install_refuses_active_new_slots(self):
        for slot in (3,4):
            with self.subTest(slot=slot), tempfile.TemporaryDirectory() as directory:
                root=Path(directory)
                image=root/"image-id.txt"
                image.write_text("sha256:"+"a"*64+"\n")
                stub=root/"stub.bash"
                stub.write_text("function /usr/bin/docker { return 0; }\n"
                    + "systemctl() { case \"$*\" in *@"+str(slot)+".service*) echo active;; *) echo inactive;; esac; }\n"
                    + "loginctl() { echo yes; }\n"
                    + "install() { echo UNEXPECTED_INSTALL >&2; return 77; }\n")
                env=dict(os.environ,HOME=str(root),BASH_ENV=str(stub))
                result=subprocess.run(["bash",str(Path(__file__).with_name("install.sh")),str(image)],env=env,capture_output=True,text=True)
                self.assertEqual(result.returncode,1,result.stderr)
                self.assertIn("runner slots before updating",result.stderr)
                self.assertNotIn("UNEXPECTED_INSTALL",result.stderr)
    def test_log_redaction(self):
        msg=m.redact("jit-value ghp_example123 Bearer hidden", "jit-value")
        self.assertNotIn("jit-value",msg);self.assertNotIn("ghp_example123",msg);self.assertNotIn("hidden",msg)
    def test_not_owned_is_never_removed(self):
        self.assertFalse(m.owned_container({"Config":{"Labels":{"orca.local-runner.owner":"other"}}},"ours"))
        self.assertTrue(m.owned_container({"Config":{"Labels":{"orca.local-runner.owner":"ours"}}},"ours"))
if __name__=="__main__":unittest.main()
