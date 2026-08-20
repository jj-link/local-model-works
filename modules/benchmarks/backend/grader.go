package backend

import "encoding/base64"

// graderImages are multi-architecture, digest-pinned GitHub Actions toolchain
// images. Each includes Python (the harness) plus the named language compiler.
var graderImages = map[string]string{
	"python":     "ghcr.io/catthehacker/ubuntu:act-latest@sha256:62d572b92f9f32d3427b6d220ad1f9dca9c7b6ffad37d295425037dbff78abaf",
	"javascript": "ghcr.io/catthehacker/ubuntu:js-latest@sha256:f764c4745b1d9a87688f3b635920b7d909b6c391ce843ec9d9f307c47b6d8e7a",
	"go":         "ghcr.io/catthehacker/ubuntu:go-latest@sha256:7fb66f0200f32c37320e03e7a33d5ad1a12e113cbe78e494174d2b34b1886037",
	"rust":       "ghcr.io/catthehacker/ubuntu:rust-latest@sha256:1d8114fb9c1fa5de85c79a31dc6d24d2128753486e46e1cbad0de98d3f9ae966",
	"java":       "ghcr.io/catthehacker/ubuntu:java-tools-latest@sha256:f453399723b2519ee6a39d83f5905b9e2f7ec668e7c26226f21b821995b84f9f",
	"cpp":        "ghcr.io/catthehacker/ubuntu:act-latest@sha256:62d572b92f9f32d3427b6d220ad1f9dca9c7b6ffad37d295425037dbff78abaf",
}

// graderScript performs inference and executes the generated implementation
// against boundary cases in the real language toolchain. It is stdlib-only;
// generated source and compiler output live on the container's bounded tmpfs.
const graderScript = `import json
import ctypes
import errno
import os
import pathlib
import platform
import re
import subprocess
import sys
import time
import urllib.request
import uuid

SPECS = {
 "python": ("Define is_leap_year(year: int) -> bool.", "solution.py", "\nassert is_leap_year(2000) and not is_leap_year(1900) and is_leap_year(2024) and not is_leap_year(2023) and is_leap_year(2400)\n", ["python3", "solution.py"]),
 "javascript": ("Define function isLeapYear(year) and export it with module.exports.", "solution.js", "\nif (!isLeapYear(2000)||isLeapYear(1900)||!isLeapYear(2024)||isLeapYear(2023)||!isLeapYear(2400)) process.exit(3);\n", ["node", "solution.js"]),
 "go": ("Return one complete package main with func IsLeapYear(year int) bool and no main function.", "main.go", "\nfunc main(){if !IsLeapYear(2000)||IsLeapYear(1900)||!IsLeapYear(2024)||IsLeapYear(2023)||!IsLeapYear(2400){panic(\"wrong\")}}\n", ["go", "run", "main.go"]),
 "rust": ("Define fn is_leap_year(year: i32) -> bool and no main function.", "main.rs", "\nfn main(){assert!(is_leap_year(2000));assert!(!is_leap_year(1900));assert!(is_leap_year(2024));assert!(!is_leap_year(2023));assert!(is_leap_year(2400));}\n", ["rustc", "main.rs", "-o", "run"]),
 "cpp": ("Define bool is_leap_year(int year) and no main function.", "main.cpp", "\nint main(){return !(is_leap_year(2000)&&!is_leap_year(1900)&&is_leap_year(2024)&&!is_leap_year(2023)&&is_leap_year(2400));}\n", ["g++", "-std=c++17", "main.cpp", "-o", "run"]),
 "java": ("Return public class LeapYear with public static boolean isLeapYear(int year).", "LeapYear.java", "", ["sh", "-lc", "javac LeapYear.java Test.java && java Test"]),
}


def extract_code(text, lang):
    fence = chr(96) * 3
    fences = re.findall(re.escape(fence) + r"(?:[A-Za-z0-9_+.-]+)?\s*\n(.*?)" + re.escape(fence), text, re.S)
    if fences:
        return max(fences, key=len).strip()
    return text.strip()


def grade(lang, code, root):
    instruction, filename, suffix, command = SPECS[lang]
    work = root / uuid.uuid4().hex
    work.mkdir()
    (work / filename).write_text(code + suffix, encoding="utf-8")
    if lang == "java":
        (work / "Test.java").write_text("public class Test { public static void main(String[] a) { if (!LeapYear.isLeapYear(2000) || LeapYear.isLeapYear(1900) || !LeapYear.isLeapYear(2024) || LeapYear.isLeapYear(2023) || !LeapYear.isLeapYear(2400)) throw new AssertionError(); }}", encoding="utf-8")
    child_env = {"PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
                 "HOME": str(work), "GOCACHE": str(work / ".go-cache"),
                 "LANG": "C.UTF-8"}
    completed = subprocess.run(command, cwd=work, stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT, text=True, timeout=90, env=child_env,
        close_fds=True)
    if completed.returncode == 0 and lang in ("rust", "cpp"):
        completed = subprocess.run([str(work / "run")], cwd=work,
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
            timeout=30, env=child_env, close_fds=True)
    return completed.returncode == 0, completed.stdout[-1200:]



def deny_network():
    syscall_by_arch = {"x86_64": 41, "amd64": 41, "aarch64": 198, "arm64": 198}
    socket_syscall = syscall_by_arch.get(platform.machine().lower())
    if socket_syscall is None:
        raise RuntimeError("unsupported sandbox architecture")
    class Filter(ctypes.Structure):
        _fields_ = [("code", ctypes.c_ushort), ("jt", ctypes.c_ubyte),
                    ("jf", ctypes.c_ubyte), ("k", ctypes.c_uint)]
    class Program(ctypes.Structure):
        _fields_ = [("len", ctypes.c_ushort), ("filter", ctypes.POINTER(Filter))]
    rules = (Filter * 4)(
        Filter(0x20, 0, 0, 0),
        Filter(0x15, 0, 1, socket_syscall),
        Filter(0x06, 0, 0, 0x00050000 | errno.EPERM),
        Filter(0x06, 0, 0, 0x7fff0000),
    )
    libc = ctypes.CDLL(None, use_errno=True)
    libc.prctl.argtypes = [ctypes.c_int, ctypes.c_ulong, ctypes.c_ulong,
                           ctypes.c_ulong, ctypes.c_ulong]
    if libc.prctl(38, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "PR_SET_NO_NEW_PRIVS")
    program = Program(len(rules), rules)
    if libc.prctl(22, 2, ctypes.addressof(program), 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "PR_SET_SECCOMP")

def post_stream(base, model, prompt, max_tokens, temperature):
    body = {"model": model, "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens, "temperature": temperature, "stream": True,
            "stream_options": {"include_usage": True}}
    req = urllib.request.Request(base.rstrip("/") + "/v1/chat/completions",
        data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    t0 = time.perf_counter(); ttft = None; last = t0; usage = None; chunks = 0; parts = []
    with urllib.request.urlopen(req, timeout=600) as resp:
        for raw in resp:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"): continue
            data = line[5:].strip()
            if data == "[DONE]": break
            try: obj = json.loads(data)
            except ValueError: continue
            if obj.get("usage"): usage = obj["usage"]
            for choice in obj.get("choices") or []:
                delta = choice.get("delta") or {}
                content = delta.get("content") or ""
                if content:
                    now = time.perf_counter()
                    if ttft is None: ttft = now - t0
                    chunks += 1; last = now; parts.append(content)
    return {"ttft": ttft, "total": time.perf_counter()-t0,
            "gen": (last-t0)-(ttft or 0), "chunks": chunks,
            "usage": usage or {}, "text": "".join(parts)}


def pct(xs, q):
    if not xs: return 0.0
    xs = sorted(xs); pos = q * (len(xs)-1); lo = int(pos); hi = min(lo+1, len(xs)-1)
    return xs[lo] + (xs[hi]-xs[lo]) * (pos-lo)


def stats(xs):
    return {"avg": sum(xs)/len(xs), "p50": pct(xs,.5), "p90": pct(xs,.9), "p99": pct(xs,.99)} if xs else {"avg":0,"p50":0,"p90":0,"p99":0}


def main():
    base=os.environ["LMW_BASE_URL"]; model=os.environ.get("LMW_MODEL","local"); lang=os.environ["LMW_LANG"]
    n=int(os.environ.get("LMW_PROMPTS","4")); max_tokens=int(os.environ.get("LMW_MAX_TOKENS","512")); temperature=float(os.environ.get("LMW_TEMPERATURE","0"))
    instruction=SPECS[lang][0]; root=pathlib.Path("/tmp/grader"); root.mkdir(parents=True,exist_ok=True)
    latencies=[]; ttfts=[]; gens=[]; programs=[]; prompt_tokens=completion_tokens=successes=passed=conn_errors=0; wall0=time.perf_counter()
    for i in range(n):
        prompt=("Implement the Gregorian leap-year rule in " + lang + ". " + instruction + " Return only one complete fenced code block. A year is a leap year when divisible by 4, except centuries unless divisible by 400. nonce=" + uuid.uuid4().hex)
        try: result=post_stream(base,model,prompt,max_tokens,temperature)
        except Exception as exc:
            print(f"[{lang} {i+1}/{n}] inference failed: {exc}",flush=True); conn_errors+=isinstance(exc,OSError); continue
        usage=result["usage"]; out_tokens=usage.get("completion_tokens") or result["chunks"]
        prompt_tokens+=usage.get("prompt_tokens") or 0; completion_tokens+=out_tokens; successes+=1
        programs.append((i, extract_code(result["text"],lang), out_tokens))
        latencies.append(result["total"]*1000); gens.append(result["gen"])
        if result["ttft"] is not None: ttfts.append(result["ttft"]*1000)
        print(f"[{lang} {i+1}/{n}] inference tokens={out_tokens}",flush=True)
    deny_network()
    for i, program, out_tokens in programs:
        try: ok, detail=grade(lang,program,root)
        except Exception as exc: ok=False; detail=str(exc)
        passed+=int(ok)
        print(f"[{lang} {i+1}/{n}] tokens={out_tokens} grade={'pass' if ok else 'fail'} {detail[-240:]}",flush=True)
    wall=time.perf_counter()-wall0; total_gen=sum(gens)
    payload={"lang":lang,"model":model,"requests":n,"successes":successes,"ok_count":passed,
      "prompt_tokens":prompt_tokens,"completion_tokens":completion_tokens,"total_tokens":prompt_tokens+completion_tokens,
      "wall_seconds":wall,"tokens_per_second":completion_tokens/total_gen if total_gen>0 else 0.0,
      "latency_ms":stats(latencies),"first_token_ms":stats(ttfts)}
    print("RESULT:"+json.dumps(payload),flush=True)
    if successes==0 and conn_errors: sys.exit(2)

if __name__ == "__main__": main()
`

var graderScriptB64 = base64.StdEncoding.EncodeToString([]byte(graderScript))
