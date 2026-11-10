#!/usr/bin/env python3
"""
Task Runner — orchestrates sequential opencode sessions for .tasks/ files.

Usage:
    python task_runner.py              # Run all pending tasks
    python task_runner.py --dry-run    # Show execution plan only
    python task_runner.py --force      # Reset completed→pending, re-run all

Reads each task file from .tasks/, resolves dependency order, then for each
pending task opens a FRESH opencode session (resetting the context window),
instructs it to implement the task following TDD, marks the task as completed,
and moves to the next.

Handles both dependency resolution and failure recovery — if a task fails,
subsequent dependent tasks are not executed.
"""

import os
import re
import subprocess
import sys
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parent
TASKS_DIR = PROJECT_ROOT / ".tasks"
MODEL = "deepseek/deepseek-flash"


# ---------------------------------------------------------------------------
# Frontmatter parsing (no PyYAML dependency)
# ---------------------------------------------------------------------------

def parse_frontmatter(content: str) -> dict | None:
    """Extract YAML frontmatter from a markdown string.

    Returns a dict with keys: id, title, status, depends_on (list), target_files (list).
    Returns None if no frontmatter block is found.
    """
    match = re.match(r"^---\s*\n(.*?)\n---", content, re.DOTALL)
    if not match:
        return None

    fm_text = match.group(1)
    fm: dict = {}
    in_list: str | None = None
    collected_list: list[str] = []

    def _flush_list():
        nonlocal in_list, collected_list
        if in_list is not None:
            fm[in_list] = collected_list
            in_list = None
            collected_list = []

    for raw in fm_text.split("\n"):
        line = raw.rstrip()

        # empty line — flush open list
        if line == "":
            _flush_list()
            continue

        # list item:   - "value"  or  - value
        list_match = re.match(r"^\s+-\s+(.*)", line)
        if list_match:
            val = list_match.group(1).strip()
            if val.startswith('"') and val.endswith('"'):
                val = val[1:-1]
            if in_list is None:
                # auto-detect: first list item under most recent key
                # (should already be set by the key line above)
                pass
            else:
                collected_list.append(val)
            continue

        # key: value  or  key:   (starts a list)
        kv = re.match(r"^(\w[\w_]*)\s*:\s*(.*)", line)
        if kv:
            _flush_list()
            key = kv.group(1)
            val = kv.group(2).strip()

            if val == "":
                # starting a block list
                in_list = key
                collected_list = []
            elif val.startswith("[") and val.endswith("]"):
                # inline list: ["a", "b"]
                inner = val[1:-1].strip()
                items = re.findall(r'"([^"]*)"', inner)
                fm[key] = items
            elif val.startswith('"') and val.endswith('"'):
                fm[key] = val[1:-1]
            else:
                fm[key] = val

    _flush_list()
    return fm


def update_status(filepath: Path, new_status: str) -> None:
    """Update the ``status`` field in the file's YAML frontmatter."""
    text = filepath.read_text()
    updated, count = re.subn(
        r"^(status:\s*)\S+",
        rf"\g<1>{new_status}",
        text,
        count=1,
        flags=re.MULTILINE,
    )
    if count == 0:
        print(f"  Warning: could not find 'status:' field in {filepath.name}")
        return
    filepath.write_text(updated)


# ---------------------------------------------------------------------------
# Task loading & ordering
# ---------------------------------------------------------------------------

def load_tasks() -> list[dict]:
    """Load all task files from .tasks/, sorted by id."""
    if not TASKS_DIR.exists():
        print(f"Error: {TASKS_DIR}/ directory not found. Run /breakdown-tasks first.")
        sys.exit(1)

    tasks = []
    for fpath in sorted(TASKS_DIR.glob("*.md")):
        fm = parse_frontmatter(fpath.read_text())
        if fm is None:
            print(f"  Warning: no frontmatter in {fpath.name} — skipping")
            continue
        tasks.append({
            "id": fm.get("id", fpath.stem),
            "title": fm.get("title", ""),
            "status": fm.get("status", "pending"),
            "depends_on": fm.get("depends_on", []) or [],
            "target_files": fm.get("target_files", []) or [],
            "filepath": fpath,
        })

    tasks.sort(key=lambda t: t["id"])
    return tasks


def resolve_order(tasks: list[dict]) -> list[dict]:
    """Topologically sort tasks by dependency order. Returns only pending tasks."""
    task_map = {t["id"]: t for t in tasks}
    completed_ids: set[str] = set()
    order: list[dict] = []

    # First pass: collect already-completed task IDs
    for t in tasks:
        if t["status"] == "completed":
            completed_ids.add(t["id"])

    # Second pass: resolve remaining
    remaining = [t for t in tasks if t["id"] not in completed_ids]
    while remaining:
        ready = [t for t in remaining
                 if all(d in completed_ids or d not in task_map
                        for d in t["depends_on"])]
        if not ready:
            stuck = [t["id"] for t in remaining]
            print(f"Error: unresolved dependencies for: {', '.join(stuck)}")
            print("Check for circular or missing prerequisites.")
            sys.exit(1)

        ready.sort(key=lambda t: t["id"])
        for t in ready:
            order.append(t)
            completed_ids.add(t["id"])
            remaining.remove(t)

    return order


# ---------------------------------------------------------------------------
# Execution
# ---------------------------------------------------------------------------

def build_prompt(task: dict) -> str:
    """Craft the one-shot message sent to opencode."""
    task_id = task["id"]
    fname = task["filepath"].name
    return (
        f"Implement task {task_id} from {TASKS_DIR.name}/{fname}. "
        f"Read the full task file first. "
        f"Follow its TDD Execution Plan EXACTLY: "
        f"Step 1 (RED) — write the failing test first and observe it fail. "
        f"Step 2 (GREEN) — write the minimum code to make the test pass. "
        f"Step 3 (REFACTOR) — clean up without breaking tests. "
        f"After the task is fully done and all tests pass, update the task "
        f"file's frontmatter: change 'status: pending' to 'status: completed'."
    )


def run_task(task: dict, dry_run: bool = False, no_commit: bool = False) -> bool:
    """Execute one task via a fresh ``opencode run`` session."""
    task_id = task["id"]
    fname = task["filepath"].name
    deps = ", ".join(task["depends_on"]) if task["depends_on"] else "none"

    bar = "─" * 58
    print(f"\n{bar}")
    print(f"  {task_id}: {task['title']}")
    print(f"  File:     {TASKS_DIR.name}/{fname}")
    print(f"  Status:   {task['status']}")
    print(f"  Deps:     {deps}")
    print(f"{bar}")

    if dry_run:
        print("  [DRY RUN] Would execute: opencode run --model "
              f"{MODEL} \"...\"")
        return True

    message = build_prompt(task)
    code = os.EX_OK

    try:
        proc = subprocess.run(
            ["opencode", "run", "--model", MODEL, message],
            cwd=PROJECT_ROOT,
        )
        code = proc.returncode
    except FileNotFoundError:
        print("\nError: 'opencode' binary not found on PATH.")
        print("Install opencode or ensure it's available in your shell.")
        return False
    except KeyboardInterrupt:
        print(f"\nInterrupted during {task_id}. Progress saved.")
        return False

    if code != 0:
        print(f"\nError: opencode exited with code {code}.")
        print(f"Task {task_id} failed — stopping runner.\n")
        return False

    # Verify the task was marked completed by opencode
    fm = parse_frontmatter(task["filepath"].read_text())
    if fm and fm.get("status") != "completed":
        print(f"  opencode completed but didn't mark task as done — updating now.")
        update_status(task["filepath"], "completed")
    else:
        print(f"  {task_id} completed and marked done by opencode.")

    if not no_commit:
        git_commit(task)

    return True


def git_commit(task: dict) -> None:
    """Stage all changes and commit with a task-specific message.

    This makes it trivial to roll back a bad task via ``git revert`` or
    ``git reset --hard HEAD~1``.
    """
    task_id = task["id"]
    title = task["title"]

    subprocess.run(["git", "add", "-A"], cwd=PROJECT_ROOT, capture_output=True)

    status = subprocess.run(
        ["git", "status", "--porcelain"],
        cwd=PROJECT_ROOT,
        capture_output=True,
        text=True,
    )
    if not status.stdout.strip():
        print(f"  Nothing to commit for {task_id}.")
        return

    commit_msg = f"{task_id}: {title}"
    result = subprocess.run(
        ["git", "commit", "-m", commit_msg],
        cwd=PROJECT_ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        print(f"  Warning: git commit failed — {result.stderr.strip()}")
    else:
        print(f"  Committed: {commit_msg}")


# ---------------------------------------------------------------------------
# CLI entry point
# ---------------------------------------------------------------------------

def main() -> None:
    dry_run = "--dry-run" in sys.argv
    # --force resets completed tasks back to pending and re-runs them
    force = "--force" in sys.argv
    # --no-commit skips git commit after each task
    no_commit = "--no-commit" in sys.argv

    tasks = load_tasks()
    if not tasks:
        print(f"No task files found in {TASKS_DIR}/")
        sys.exit(0)

    print(f"Found {len(tasks)} task(s) in {TASKS_DIR}/")
    for t in tasks:
        icon = "✓" if t["status"] == "completed" else "○"
        deps = ", ".join(t["depends_on"]) if t["depends_on"] else "—"
        print(f"  {icon} {t['id']}  {t['title']}")
        print(f"       status={t['status']}  deps=[{deps}]")

    # Mark all as pending if --force
    if force:
        for t in tasks:
            if t["status"] == "completed":
                update_status(t["filepath"], "pending")
                t["status"] = "pending"
        print("\n--force: reset all tasks to pending.")

    order = resolve_order(tasks)

    if not order:
        print("\nAll tasks already completed. Nothing to do.")
        sys.exit(0)

    print(f"\nExecution order ({len(order)} tasks):")
    for i, t in enumerate(order, 1):
        print(f"  {i}. {t['id']}: {t['title']}")

    if dry_run:
        print("\n--dry-run: no tasks will be executed.")
        for t in order:
            run_task(t, dry_run=True)
        sys.exit(0)

    if no_commit:
        print("\n--no-commit: git commits disabled.")

    print(f"\nStarting execution... (Ctrl+C to stop between tasks)")
    succeeded = 0
    for t in order:
        if run_task(t, no_commit=no_commit):
            succeeded += 1
        else:
            print(f"\nRunner stopped — {succeeded} task(s) completed, "
                  f"{len(order) - succeeded} remaining.")
            sys.exit(1)

    print(f"\n{'─' * 58}")
    print(f"  All {succeeded} task(s) completed successfully.")
    print(f"{'─' * 58}")


if __name__ == "__main__":
    main()
