#!/usr/bin/env python3
"""
External Batch Auto-Resume Tool for edualc (`edualc resume`).

Scans Claude Desktop session JSONL files under data/desktop/.claude/projects/,
identifies interrupted/stalled sessions (e.g. stopped after tool_result, error,
or missing final assistant reply), waits for the 10/10 safety gate if tripped,
and resumes selected sessions cleanly inside the isolated `mini-cage-relay`
container via `claude --resume <id> -p ...`, syncing the updated transcript
back so Claude Desktop shows the completed results.
"""

import argparse
import concurrent.futures
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request
from datetime import datetime
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent.parent
DESKTOP_PROJECTS_DIR = ROOT_DIR / "data" / "desktop" / ".claude" / "projects"
ACCOUNTS_DIR = ROOT_DIR / "data" / "accounts"


def read_host_port() -> str:
    env_file = ROOT_DIR / ".env"
    if env_file.exists():
        for line in env_file.read_text(encoding="utf-8", errors="ignore").splitlines():
            if line.startswith("HOST_PORT="):
                v = line.split("=", 1)[1].strip()
                if v:
                    return v
    return "8317"


def wait_for_safety_gate(port: str, wait_sec: int = 300) -> bool:
    url = f"http://127.0.0.1:{port}/health"
    deadline = time.time() + wait_sec
    warned = False
    while True:
        try:
            with urllib.request.urlopen(url, timeout=5) as r:
                if r.status == 200:
                    if warned:
                        print("[batch-resume] 安全门控已恢复 10/10 放行，继续执行！", flush=True)
                    return True
        except Exception:
            pass
        if time.time() >= deadline:
            return False
        if not warned:
            print("[batch-resume] 安全门控当前未放行，等待自动恢复绿灯...", flush=True)
            warned = True
        time.sleep(5)


def extract_text(content) -> str:
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        parts = []
        for item in content:
            if isinstance(item, dict):
                if item.get("type") == "text" and item.get("text"):
                    parts.append(str(item["text"]))
        return " ".join(parts).strip()
    return ""


def inspect_session(jsonl_path: Path) -> dict:
    stat = jsonl_path.stat()
    mtime = stat.st_mtime
    age_sec = time.time() - mtime

    title = ""
    last_role = ""
    last_kind = ""
    last_snippet = ""
    has_pending_tool = False
    had_error = False

    try:
        with jsonl_path.open("r", encoding="utf-8", errors="ignore") as f:
            lines = f.readlines()
    except OSError:
        return {}

    for line in lines:
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            j = json.loads(line)
        except Exception:
            continue

        t = j.get("type", "")
        if t == "custom-title" and j.get("customTitle"):
            title = str(j["customTitle"]).strip()

        msg = j.get("message")
        if not isinstance(msg, dict):
            continue
        role = msg.get("role", "")
        content = msg.get("content")

        if not title and role == "user":
            txt = extract_text(content)
            if txt and not txt.startswith("<"):
                title = txt.replace("\n", " ")[:48]

        if t not in ("user", "assistant"):
            continue

        last_role = role
        if isinstance(content, list):
            kinds = [c.get("type") for c in content if isinstance(c, dict)]
            if "tool_use" in kinds:
                last_kind = "tool_use"
                has_pending_tool = True
            elif "tool_result" in kinds:
                last_kind = "tool_result"
                has_pending_tool = True
                for c in content:
                    if isinstance(c, dict) and c.get("is_error"):
                        had_error = True
            elif "text" in kinds:
                last_kind = "text"
                has_pending_tool = False
                txt = extract_text(content)
                last_snippet = txt.replace("\n", " ")[:80]
                had_error = ("No response requested" in txt or "[Tool call interrupted" in txt)
            elif "thinking" in kinds:
                last_kind = "thinking"
                has_pending_tool = True
            else:
                last_kind = "other"
                has_pending_tool = False
        elif isinstance(content, str):
            last_kind = "text"
            has_pending_tool = False
            last_snippet = content.replace("\n", " ")[:80]
            had_error = ("No response requested" in content or "[Tool call interrupted" in content)

    # Classify state
    if age_sec < 45:
        status = "ACTIVE"
        reason = "最近45秒仍在写入"
    elif last_role == "user" or has_pending_tool or last_kind in ("tool_use", "tool_result", "thinking"):
        status = "STALLED"
        reason = f"中断于 {last_role}/{last_kind}（未输出最终回复）"
    elif had_error:
        status = "STALLED"
        reason = "上次工具调用被中断或无响应"
    else:
        status = "DONE"
        reason = "已有最终助手回复"

    return {
        "id": jsonl_path.stem,
        "project_slug": jsonl_path.parent.name,
        "path": jsonl_path,
        "title": title or jsonl_path.stem[:12],
        "status": status,
        "reason": reason,
        "last_snippet": last_snippet,
        "mtime": mtime,
        "mtime_str": datetime.fromtimestamp(mtime).strftime("%m-%d %H:%M"),
        "size_kb": stat.st_size // 1024,
    }


def scan_sessions(project_filter: str = "", hours: float = 48.0) -> list[dict]:
    if not DESKTOP_PROJECTS_DIR.exists():
        return []
    cutoff = time.time() - hours * 3600
    items = []
    for proj_dir in DESKTOP_PROJECTS_DIR.iterdir():
        if not proj_dir.is_dir():
            continue
        if project_filter and project_filter.lower() not in proj_dir.name.lower():
            continue
        for jsonl in proj_dir.glob("*.jsonl"):
            if jsonl.stat().st_mtime < cutoff:
                continue
            info = inspect_session(jsonl)
            if info:
                items.append(info)
    items.sort(key=lambda x: x["mtime"], reverse=True)
    return items


def resume_one(session: dict, account: str, prompt: str, port: str, dry_run: bool, timeout_sec: int) -> tuple[str, bool, str]:
    sid = session["id"]
    slug = session["project_slug"]
    src_jsonl: Path = session["path"]
    src_sub = src_jsonl.parent / sid

    dst_proj = ACCOUNTS_DIR / account / "projects" / slug
    dst_jsonl = dst_proj / src_jsonl.name
    dst_sub = dst_proj / sid

    if dry_run:
        return sid, True, f"[dry-run] 将续跑 {sid[:8]} ({session['title']})"

    if not wait_for_safety_gate(port):
        return sid, False, "等待安全门控放行超时，已跳过"

    dst_proj.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src_jsonl, dst_jsonl)
    if src_sub.is_dir():
        if dst_sub.exists():
            shutil.rmtree(dst_sub)
        shutil.copytree(src_sub, dst_sub)

    # Infer container cwd from project slug (e.g. -workspace-myapp -> /workspace/myapp)
    guest_cwd = "/workspace"
    if slug.startswith("-workspace-"):
        guest_cwd = "/workspace/" + slug[len("-workspace-"):]

    cmd = [
        "docker", "exec",
        "-e", f"ACCOUNT={account}",
        "mini-cage-relay",
        "/usr/local/bin/airlock-shell.sh",
        "bash", "-lc",
        f'cd "{guest_cwd}" && claude --resume "{sid}" -p {json.dumps(prompt)} --permission-mode acceptEdits --allowedTools "Bash,Read,Edit,Write,Glob,Grep,Agent,NotebookEdit" --output-format text',
    ]

    started = time.time()
    print(f"[batch-resume] 开始续跑 {sid[:8]} · {session['title']} ...", flush=True)
    try:
        res = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, timeout=timeout_sec)
    except subprocess.TimeoutExpired:
        return sid, False, f"续跑超时（>{timeout_sec}s）"

    elapsed = int(time.time() - started)

    # Sync updated transcript back to Claude Desktop project directory
    if dst_jsonl.exists() and dst_jsonl.stat().st_mtime >= src_jsonl.stat().st_mtime:
        shutil.copy2(dst_jsonl, src_jsonl)
        if dst_sub.is_dir():
            shutil.copytree(dst_sub, src_sub, dirs_exist_ok=True)
        # Update matching Claude Desktop local_*.json lastActivityAt so sidebar reflects fresh turn
        sessions_root = ROOT_DIR / "data" / "desktop" / ".config" / "Claude" / "claude-code-sessions"
        if sessions_root.exists():
            now_ms = int(time.time() * 1000)
            for meta_file in sessions_root.rglob("local_*.json"):
                try:
                    raw = meta_file.read_text(encoding="utf-8", errors="ignore")
                    if sid not in raw:
                        continue
                    meta = json.loads(raw)
                    if meta.get("cliSessionId") == sid:
                        meta["lastActivityAt"] = now_ms
                        meta["lastTurnEdgeAt"] = now_ms
                        meta_file.write_text(json.dumps(meta, ensure_ascii=False), encoding="utf-8")
                        break
                except Exception:
                    pass
        # Ensure desktop container's non-root airlock user retains ownership
        subprocess.run(
            ["docker", "exec", "-u", "root", "mini-cage-desktop",
             "chown", "-R", "airlock:airlock", f"/data/desktop/.claude/projects/{slug}", "/data/desktop/.config/Claude/claude-code-sessions"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )

    if res.returncode != 0:
        err = (res.stderr or res.stdout or "").strip().splitlines()
        tail = err[-1] if err else f"exit={res.returncode}"
        return sid, False, f"失败 ({elapsed}s): {tail[:160]}"

    out_lines = [ln.strip() for ln in (res.stdout or "").splitlines() if ln.strip()]
    summary = out_lines[-1][:140] if out_lines else "已完成"
    return sid, True, f"完成 ({elapsed}s) · {summary}"


def print_table(sessions: list[dict]) -> None:
    if not sessions:
        print("未找到近期 Claude Desktop 会话记录。")
        return
    print(f"{'ID(前8位)':<10} {'状态':<8} {'更新时间':<12} {'大小':<7} {'标题 / 原因'}")
    print("-" * 92)
    for s in sessions:
        tag = {"STALLED": "中断待续", "ACTIVE": "运行中", "DONE": "已完成"}.get(s["status"], s["status"])
        print(f"{s['id'][:8]:<10} {tag:<8} {s['mtime_str']:<12} {str(s['size_kb'])+'KB':<7} {s['title']}  [{s['reason']}]")


def main() -> int:
    parser = argparse.ArgumentParser(description="edualc 外置批量续跑工具（扫描并自动续跑中断的 Claude Desktop 会话）")
    parser.add_argument("ids", nargs="*", help="指定要续跑的会话 ID（支持前 8 位前缀）；不填且配合 --all 则续跑全部中断会话")
    parser.add_argument("--list", action="store_true", help="仅列出近期会话与中断状态")
    parser.add_argument("--json", action="store_true", help="以 JSON 格式输出近期会话列表")
    parser.add_argument("--all", action="store_true", help="批量续跑所有状态为「中断待续 (STALLED)」的会话")
    parser.add_argument("--watch", action="store_true", help="守护模式：每 60 秒巡检一次，发现中断且门控绿灯时自动续跑")
    parser.add_argument("--account", default="default", help="使用的 CLI 账号槽位（默认 default）")
    parser.add_argument("--project", default="", help="按项目目录名过滤（如 myapp）")
    parser.add_argument("--hours", type=float, default=48.0, help="扫描最近多少小时内的会话（默认 48）")
    parser.add_argument("--concurrency", type=int, default=1, help="并发续跑数（默认 1 串行最稳，避免抢构建锁）")
    parser.add_argument("--timeout", type=int, default=900, help="单个会话续跑超时秒数（默认 900）")
    parser.add_argument("--prompt", default="继续完成上面中断的任务，并给出简明的最终结果总结。", help="发送给中断会话的续跑提示词")
    parser.add_argument("--dry-run", action="store_true", help="仅预览将要续跑的会话，不实际执行")
    args = parser.parse_args()

    port = read_host_port()

    if args.watch:
        print(f"[batch-resume] 启动守护自动续跑（账号={args.account}，每 60 秒巡检中断会话）...", flush=True)
        while True:
            sessions = scan_sessions(args.project, args.hours)
            stalled = [s for s in sessions if s["status"] == "STALLED"]
            if stalled:
                print(f"[batch-resume] 检测到 {len(stalled)} 个中断会话，开始逐个续跑...", flush=True)
                for s in stalled:
                    sid, ok, msg = resume_one(s, args.account, args.prompt, port, args.dry_run, args.timeout)
                    print(f"  {'[OK]' if ok else '[ERR]'} {sid[:8]}: {msg}", flush=True)
            time.sleep(60)

    sessions = scan_sessions(args.project, args.hours)
    if args.json:
        safe_sessions = [{k: v for k, v in s.items() if k != "path"} for s in sessions]
        print(json.dumps({
            "sessions": safe_sessions,
            "stalled_count": sum(1 for s in sessions if s["status"] == "STALLED"),
        }, ensure_ascii=False))
        return 0
    if args.list or (not args.all and not args.ids):
        print_table(sessions)
        stalled_cnt = sum(1 for s in sessions if s["status"] == "STALLED")
        print(f"\n共 {len(sessions)} 个近期会话，其中 {stalled_cnt} 个处于「中断待续」状态。")
        if stalled_cnt > 0:
            print("批量续跑全部中断会话：  edualc resume --all")
            print("仅续跑指定会话：        edualc resume <前8位ID> [<前8位ID>...]")
            print("后台守护自动续跑：      edualc resume --watch")
        return 0

    targets = []
    if args.ids:
        for prefix in args.ids:
            matched = [s for s in sessions if s["id"].startswith(prefix)]
            if not matched:
                print(f"[warn] 未找到匹配前缀 {prefix!r} 的会话", file=sys.stderr)
            targets.extend(matched)
    elif args.all:
        targets = [s for s in sessions if s["status"] == "STALLED"]

    if not targets:
        print("没有需要续跑的中断会话。")
        return 0

    print(f"准备批量续跑 {len(targets)} 个会话（并发={args.concurrency}，账号={args.account}）：")
    for s in targets:
        print(f"  - {s['id'][:8]} | {s['title']} ({s['reason']})")
    print("")

    conc = max(1, min(args.concurrency, 3))
    failed = 0
    if conc == 1:
        for s in targets:
            sid, ok, msg = resume_one(s, args.account, args.prompt, port, args.dry_run, args.timeout)
            print(f"{'[OK]' if ok else '[ERR]'} {sid[:8]} | {msg}", flush=True)
            if not ok:
                failed += 1
    else:
        with concurrent.futures.ThreadPoolExecutor(max_workers=conc) as pool:
            futs = [pool.submit(resume_one, s, args.account, args.prompt, port, args.dry_run, args.timeout) for s in targets]
            for fut in concurrent.futures.as_completed(futs):
                sid, ok, msg = fut.result()
                print(f"{'[OK]' if ok else '[ERR]'} {sid[:8]} | {msg}", flush=True)
                if not ok:
                    failed += 1

    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
