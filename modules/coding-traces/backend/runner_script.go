package backend

// runnerBootstrap executes inside a fresh digest-pinned SWE-Gym instance image.
// Credentials arrive over the agent-owned Unix socket and are held only in this
// Python process; no credential is written to a file or environment variable.
const runnerBootstrap = `
import base64, glob, json, os, pathlib, socket, subprocess, sys, tarfile, tempfile, urllib.request
TASK=json.loads(base64.b64decode(sys.argv[1]))
CONFIG=json.loads(base64.b64decode(sys.argv[2]))
SAMPLING=json.loads(base64.b64decode(sys.argv[3]))
SOCKET='/run/lmw/trace.sock'
sock=socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); sock.connect(SOCKET)
stream=sock.makefile('rwb', buffering=0)
credentials=json.loads(stream.readline())['values']
sequence=0
def scrub_reasoning(value):
 if CONFIG.get('capture_reasoning',True): return value
 if isinstance(value,dict): return {key:scrub_reasoning(item) for key,item in value.items() if key.lower() not in ('reasoning','reasoning_content','analysis','thought','thinking')}
 if isinstance(value,list): return [scrub_reasoning(item) for item in value]
 return value
def emit(kind,payload,input_tokens=0,output_tokens=0):
 global sequence
 event={'sequence':sequence,'event_id':'runner-%08d'%sequence,'occurred_at':__import__('datetime').datetime.now(__import__('datetime').timezone.utc).isoformat().replace('+00:00','Z'),'kind':kind,'payload':scrub_reasoning(payload),'input_tokens':int(input_tokens or 0),'output_tokens':int(output_tokens or 0)}
 stream.write((json.dumps(event,separators=(',',':'),sort_keys=True)+'\n').encode()); sequence+=1
emit('agent.lifecycle',{'state':'starting','schema':'localmodelworks/agent-trace/v1'})
root='/opt/lmw-openhands-` + openHandsCommit + `'
if not pathlib.Path(root).exists():
 archive='/tmp/openhands.tar.gz'
 urllib.request.urlretrieve('https://github.com/SWE-Gym/OpenHands/archive/` + openHandsCommit + `.tar.gz',archive)
 with tarfile.open(archive,'r:gz') as tf: tf.extractall('/opt')
 extracted=glob.glob('/opt/OpenHands-*')[0]; os.rename(extracted,root)
 subprocess.check_call([sys.executable,'-m','pip','install','-q','-e',root])
os.chdir(root); sys.path.insert(0,root)
import pandas as pd
from openhands.core.config import LLMConfig
from evaluation.utils.shared import EvalMetadata
import evaluation.swe_bench.run_infer as infer
infer.USE_HINT_TEXT=False; infer.RUN_WITH_BROWSING=False
secret=credentials.get(CONFIG.get('secret_reference',''))
runtime_secret=credentials.get(CONFIG.get('runtime_secret_reference',''))
llm=LLMConfig(model=SAMPLING['model'],api_key=secret or 'local',base_url=CONFIG.get('endpoint'),temperature=SAMPLING['temperature'],max_input_tokens=CONFIG.get('context_limit'),max_output_tokens=CONFIG.get('output_limit'),log_completions=True,log_completions_folder='/tmp/lmw-completions')
metadata=EvalMetadata(agent_class='CodeActAgent',llm_config=llm,max_iterations=SAMPLING['max_turns'],eval_output_dir='/tmp/lmw-eval',start_time='',git_commit='` + openHandsCommit + `',dataset='SWE-Gym',data_split='train',details={'hints_enabled':False,'browsing_enabled':False})
original_get_config=infer.get_config
def lmw_get_config(instance,metadata):
 config=original_get_config(instance,metadata)
 if CONFIG.get('runtime')=='openhands_remote':
  config.runtime='remote'; config.sandbox.api_key=runtime_secret; config.sandbox.remote_runtime_api_url=CONFIG.get('runtime_endpoint')
 else: config.runtime='local'
 return config
infer.get_config=lmw_get_config
row={'instance_id':TASK['instance_id'],'problem_statement':TASK['problem_statement'],'repo':TASK['repo'],'base_commit':TASK['base_commit'],'version':TASK.get('version',''),'FAIL_TO_PASS':TASK['fail_to_pass'],'PASS_TO_PASS':TASK['pass_to_pass'],'test_patch':TASK.get('test_patch',''),'patch':'','hints_text':''}
try:
 output=infer.process_instance(pd.Series(row),metadata,reset_logger=False)
 raw=output.model_dump() if hasattr(output,'model_dump') else output.dict()
 history=raw.get('history') or []
 for item in history:
  name=str(item.get('action') or item.get('observation') or item.get('type') or '').lower()
  kind='message' if 'message' in name else ('tool.result' if item.get('observation') or 'observation' in name else 'tool.call')
  emit(kind,item)
 for path in sorted(glob.glob('/tmp/lmw-completions/**/*',recursive=True)):
  if os.path.isfile(path):
   try:
    completion=json.load(open(path,encoding='utf-8'))
    usage=completion.get('usage') or {}; emit('model.response',completion,usage.get('prompt_tokens',0),usage.get('completion_tokens',0))
   except Exception: pass
 patch=subprocess.run(['git','-C','/testbed','diff','--binary','--full-index',TASK['base_commit']],capture_output=True,text=True).stdout
 emit('agent.lifecycle',{'state':'completed','error':raw.get('error')})
 print('LMW_RESULT:'+json.dumps({'git_patch':patch,'error':raw.get('error'),'metrics':raw.get('metrics')},separators=(',',':')),flush=True)
except Exception as exc:
 emit('agent.lifecycle',{'state':'failed','error':str(exc)})
 print('LMW_RESULT:'+json.dumps({'git_patch':'','error':str(exc),'infrastructure_error':True},separators=(',',':')),flush=True)
 raise
finally:
 for key in list(credentials): credentials[key]=''; del credentials[key]
 stream.close(); sock.close()
`

const graderBootstrap = `
import base64, glob, json, os, pathlib, subprocess, sys, tarfile, types, urllib.request
TASK=json.loads(base64.b64decode(sys.argv[1])); PATCH=base64.b64decode(sys.argv[2]).decode()
root='/opt/lmw-swebench-` + sweBenchCommit + `'
if not pathlib.Path(root).exists():
 archive='/tmp/swebench.tar.gz'; urllib.request.urlretrieve('https://github.com/SWE-Gym/SWE-Bench-Fork/archive/` + sweBenchCommit + `.tar.gz',archive)
 with tarfile.open(archive,'r:gz') as tf: tf.extractall('/opt')
 extracted=glob.glob('/opt/SWE-Bench-Fork-*')[0]; os.rename(extracted,root)
 open(root+'/swebench/__init__.py','w').write('')
sys.path.insert(0,root)
# The grading path uses only harness modules. Stub dataset-card helpers so
# importing the pinned fork cannot initialize Hugging Face state in the image.
datasets_stub=types.ModuleType('datasets'); datasets_stub.Dataset=object; datasets_stub.load_dataset=lambda *args,**kwargs: None; sys.modules['datasets']=datasets_stub
dotenv_stub=types.ModuleType('dotenv'); dotenv_stub.load_dotenv=lambda *args,**kwargs: None; sys.modules['dotenv']=dotenv_stub
from swebench.harness.test_spec import make_test_spec
from swebench.harness.grading import get_eval_report
row={'instance_id':TASK['instance_id'],'problem_statement':TASK['problem_statement'],'repo':TASK['repo'],'base_commit':TASK['base_commit'],'version':TASK.get('version',''),'FAIL_TO_PASS':TASK['fail_to_pass'],'PASS_TO_PASS':TASK['pass_to_pass'],'test_patch':TASK.get('test_patch',''),'patch':'','hints_text':''}
status='unresolved'; failure_kind=None; stdout=''; stderr=''; exit_status=None; report={}
if not PATCH.strip(): failure_kind='empty_patch'
else:
 open('/tmp/model.patch','w').write(PATCH)
 applied=subprocess.run("cd /testbed && (git apply -v /tmp/model.patch || patch --batch --fuzz=5 -p1 -i /tmp/model.patch)",shell=True,capture_output=True,text=True)
 stdout+=applied.stdout; stderr+=applied.stderr
 if applied.returncode!=0: failure_kind='apply_failure'; exit_status=applied.returncode
 else:
  spec=make_test_spec(row); open('/tmp/eval.sh','w').write(spec.eval_script); os.chmod('/tmp/eval.sh',0o700)
  try:
   evaluated=subprocess.run(['/tmp/eval.sh'],cwd='/testbed',capture_output=True,text=True,timeout=int(sys.argv[3]))
   stdout+=evaluated.stdout; stderr+=evaluated.stderr; exit_status=evaluated.returncode
   log_dir='/tmp/logs/'+TASK['instance_id']; os.makedirs(log_dir,exist_ok=True); log_path=log_dir+'/test_output.txt'
   open(log_path,'w').write('APPLY_PATCH_PASS\n'+evaluated.stdout+'\n'+evaluated.stderr)
   prediction={'instance_id':TASK['instance_id'],'model_name_or_path':'lmw','model_patch':PATCH}
   report=get_eval_report(spec,prediction,log_path,True).get(TASK['instance_id'],{})
   if report.get('resolved') is True: status='resolved'
   elif evaluated.returncode!=0: failure_kind='tests_failed'
  except subprocess.TimeoutExpired as exc:
   stdout+=exc.stdout.decode() if isinstance(exc.stdout,bytes) else (exc.stdout or '')
   stderr+=exc.stderr.decode() if isinstance(exc.stderr,bytes) else (exc.stderr or '')
   failure_kind='timeout'
result={'status':status,'failure_kind':failure_kind,'exit_status':exit_status,'stdout':stdout,'stderr':stderr,'report':report,'fail_to_pass_report':(report.get('tests_status') or {}).get('FAIL_TO_PASS',{}),'pass_to_pass_report':(report.get('tests_status') or {}).get('PASS_TO_PASS',{})}
print('LMW_GRADE_RESULT:'+json.dumps(result,separators=(',',':')),flush=True)
`
