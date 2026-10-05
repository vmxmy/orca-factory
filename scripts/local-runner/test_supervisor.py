import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch
import tempfile
import json
import subprocess
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
        for n in [0,3,100]:
            with self.assertRaises(ValueError):m.validate_workers(n)
        for n in [1,2]:m.validate_workers(n)
    def test_log_redaction(self):
        msg=m.redact("jit-value ghp_example123 Bearer hidden", "jit-value")
        self.assertNotIn("jit-value",msg);self.assertNotIn("ghp_example123",msg);self.assertNotIn("hidden",msg)
    def test_not_owned_is_never_removed(self):
        self.assertFalse(m.owned_container({"Config":{"Labels":{"orca.local-runner.owner":"other"}}},"ours"))
        self.assertTrue(m.owned_container({"Config":{"Labels":{"orca.local-runner.owner":"ours"}}},"ours"))
if __name__=="__main__":unittest.main()
