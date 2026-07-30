#!/usr/bin/env python3
"""Seed "Demo Exam — completed" against a running dev server (demo-polish plan
2026-07-10, Task SEED).

Drives ONLY the public HTTP API (dev login as the bootstrap admin) end to end:
assessment + problems + rubrics + reference solutions → per-student PDF
submissions (regenerated deterministically from make-demo-data.py's SEED=46
content) → masking applied and bulk-accepted → answer rows materialized for the
whole roster → an AI grading run on the cheapest configured method → spot-check
verdicts → final source → publish (file email provider writes data/outbox/) →
two regrade threads filed through the real inbound webhook. Doubling as an
end-to-end smoke test: it asserts coverage/publish/regrade state at the end.

Run via: make demo-walkthrough
   (i.e. uv run --with reportlab python scripts/seed-demo-walkthrough.py)

Idempotence is skip-not-merge: if the assessment already exists, print its
state and exit 0. Flags:
  --no-ai           stop before the AI run (steps 1-4 only) with instructions
  --continue        resume steps 5-7 (run/publish/regrades) on the existing
                    assessment — the counterpart of --no-ai; each step is
                    skipped when its outcome already exists
  --regrades-only   skip to the regrade step against the existing assessment
                    (for after a server restart that enabled the webhook)

The demo roster is synthetic (never real people); still, prints stay to
counts/ids per the repo PII rule.
"""

import argparse
import importlib.util
import io
import json
import os
import random
import re
import sys
import time
import urllib.error
import urllib.request
import uuid
from email import policy
from email.parser import BytesParser
from http.cookiejar import CookieJar
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# make-demo-data.py is imported as a module (not duplicated): its STUDENTS/
# PROBLEMS/SEED constants and drawing helpers are the single source of truth
# for the demo content. Importing it registers the CJK font; it writes nothing
# (file output happens only in its main()).
_spec = importlib.util.spec_from_file_location(
    "make_demo_data", ROOT / "scripts" / "make-demo-data.py"
)
mdd = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mdd)

from reportlab.pdfgen import canvas  # noqa: E402  (needs --with reportlab)

ASSESSMENT_NAME = "Demo Exam — completed"
DEFAULT_BASE = os.environ.get("ADAMARKER_BASE_URL", "http://localhost:8899")
ADMIN_EMAIL = os.environ.get("ADAMARKER_BOOTSTRAP_ADMIN_EMAIL", "b11902156@ntu.edu.tw")
WEBHOOK_SECRET = os.environ.get("ADAMARKER_INBOUND_WEBHOOK_SECRET", "dev-webhook-secret")
OUTBOX = Path(os.environ.get("ADAMARKER_EMAIL_OUTBOX_DIR", ROOT / "data" / "outbox"))

# Pricing for the curated cheap models (docs/MODELS.md) — entered before the AI
# run when the chosen model has no pricing row, so the run's cost_usd is real
# rather than NULL-summed-to-0 (trust spec §2: missing pricing ⇒ NULL cost).
KNOWN_PRICING_USD_PER_MTOK = {
    "qwen/qwen3.5-flash-02-23": ("0.065", "0.26"),
    "google/gemini-3.1-flash-lite": ("0.25", "1.50"),
    "openai/gpt-5-nano": ("0.05", "0.40"),
}

# Rubrics written to split the demo answers' quality tiers (GOOD ≈ full marks,
# WRONG fails the core criterion, SHORT earns only the headline point, blank/
# garbled ≈ 0). Criterion points sum to the problem max (10) — enforced
# server-side (D4).
RUBRICS = {
    "Q1": [
        ("States the correct worst-case bound O(log n)", "3",
         "0 for O(n) or a best-case-only claim."),
        ("Solves the recurrence T(n)=T(n/2)+O(1): log2 n halvings, O(1) work each", "4",
         "2 if the recurrence is named but never unrolled/solved."),
        ("Identifies the worst case (absent element; ~log2 n + 1 probes until the range is empty)", "3",
         "1 for a vague 'when the element is missing' remark with no probe count."),
    ],
    "Q2": [
        ("Uses the earliest-finish-time greedy and sets up greedy-vs-OPT", "3",
         "0 for a different greedy (e.g. shortest interval first) — that rule is wrong."),
        ("Exchange/induction argument f(g_i) <= f(o_i) carried through", "4",
         "2 if the swap idea is stated but compatibility after the swap is never argued."),
        ("Concludes |greedy| >= |OPT|, so greedy is maximum-size", "3",
         "1 if optimality is asserted without connecting the induction to the sizes."),
    ],
    "Q3": [
        ("Defines the subproblem L[i] = LIS ending exactly at a[i]", "3",
         "0 for 'sort the array' — that solves a different problem."),
        ("Correct recurrence L[i] = 1 + max L[j] over j<i with a[j]<a[i]; answer = max L[i]", "4",
         "2 if the recurrence misses the a[j]<a[i] condition or the final max."),
        ("Running time stated correctly: O(n^2), or O(n log n) via tails + binary search", "3",
         "Full 3 only when the claimed bound matches the algorithm actually given."),
    ],
    "Q4": [
        ("Correct verdict: the claim is TRUE for unweighted BFS", "3",
         "0 for 'False' — enqueue order cannot make a longer path win in an unweighted graph."),
        ("States the layer invariant (after layer d, every distance-d vertex has dist = d)", "4",
         "2 for an informal 'BFS explores level by level' with no invariant."),
        ("Induction step: a distance-(d+1) vertex is discovered from a layer-d neighbour", "3",
         "1 if the step is asserted without the neighbour argument."),
    ],
}


def log(msg):
    print(msg, flush=True)


def die(msg):
    print(f"FATAL: {msg}", file=sys.stderr, flush=True)
    sys.exit(1)


class API:
    """Tiny stdlib client: cookie jar + X-ADA-CSRF on mutating calls (D7)."""

    def __init__(self, base):
        self.base = base.rstrip("/")
        self.opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(CookieJar())
        )

    def request(self, method, path, body=None, content_type=None):
        headers = {}
        data = None
        if body is not None:
            if isinstance(body, (dict, list)):
                data = json.dumps(body).encode("utf-8")
                headers["Content-Type"] = "application/json"
            else:
                data = body
                headers["Content-Type"] = content_type or "application/octet-stream"
        if method not in ("GET", "HEAD"):
            headers["X-ADA-CSRF"] = "1"
        req = urllib.request.Request(
            self.base + path, data=data, headers=headers, method=method
        )
        try:
            with self.opener.open(req, timeout=300) as resp:
                raw = resp.read()
                status = resp.status
        except urllib.error.HTTPError as e:
            raw = e.read()
            status = e.code
        except urllib.error.URLError as e:
            die(f"{method} {path}: cannot reach server ({e.reason}) — is the dev server on {self.base} running?")
        payload = None
        if raw:
            try:
                payload = json.loads(raw)
            except ValueError:
                payload = raw.decode("utf-8", "replace")
        return status, payload

    def call(self, method, path, body=None, content_type=None, ok=(200, 201, 202, 204)):
        status, payload = self.request(method, path, body, content_type)
        if status not in ok:
            die(f"{method} {path} -> {status}: {payload}")
        return payload

    def get(self, path):
        return self.call("GET", path)

    def post(self, path, body=None, **kw):
        return self.call("POST", path, body, **kw)

    def put(self, path, body=None):
        return self.call("PUT", path, body)


def poll(desc, fn, timeout, interval=3, fatal=True):
    """fn() -> (done, state). Returns state when done; on timeout dies, or
    returns None when fatal=False (caller handles the miss)."""
    deadline = time.time() + timeout
    while True:
        done, state = fn()
        if done:
            return state
        if time.time() > deadline:
            if fatal:
                die(f"timed out after {timeout}s waiting for {desc} (last: {state})")
            return None
        time.sleep(interval)


# --- deterministic demo content ------------------------------------------------------


def demo_answer_bodies():
    """Recompute the SEED=46 pile's per-(student, problem) answer bodies by
    replaying make_pile's exact rng sequence (shuffle, then answer_for per page
    in shuffled order) — so the per-student PDFs carry the same answers as the
    committed demo-scan-pile.pdf."""
    rng = random.Random(mdd.SEED)
    pages = [(sid, name, code) for sid, name in mdd.STUDENTS for code, _, _ in mdd.PROBLEMS]
    rng.shuffle(pages)
    bodies = {}
    for sid, _name, code in pages:
        bodies[(sid, code)] = mdd.answer_for(rng, code)
    return bodies


def student_pdf(sid, name, bodies):
    """One 4-page PDF, page i = problem Qi+1 (the Submissions path maps page i
    onto the problem at position i — internal/ingest positional mapping)."""
    buf = io.BytesIO()
    c = canvas.Canvas(buf, pagesize=(mdd.W, mdd.H), invariant=1)
    for code, _title, _body in mdd.PROBLEMS:
        mdd.draw_header(c, sid, name, code)
        mdd.wrap_lines(c, bodies[(sid, code)], mdd.W * 0.1, mdd.H * 0.8,
                       leading=22, font=("Helvetica", 12))
        c.showPage()
    c.save()
    return buf.getvalue()


def multipart_body(files):
    """files: [(filename, bytes)] under form field 'files'."""
    boundary = "adamarker-seed-" + uuid.uuid4().hex
    out = io.BytesIO()
    for filename, data in files:
        out.write(f"--{boundary}\r\n".encode())
        out.write(
            f'Content-Disposition: form-data; name="files"; filename="{filename}"\r\n'
            "Content-Type: application/pdf\r\n\r\n".encode()
        )
        out.write(data)
        out.write(b"\r\n")
    out.write(f"--{boundary}--\r\n".encode())
    return out.getvalue(), f"multipart/form-data; boundary={boundary}"


# --- steps ---------------------------------------------------------------------------


def find_assessment(api, name):
    for a in api.get("/api/assessments")["assessments"]:
        if a["name"] == name:
            return a
    return None


def create_structure(api):
    a = api.post("/api/assessments", {"kind": "exam", "name": ASSESSMENT_NAME})
    aid = a["id"] if "id" in a else a["assessment"]["id"]
    log(f"[1] created assessment {aid} ({ASSESSMENT_NAME!r})")

    problems = {}
    for i, (code, title, statement) in enumerate(mdd.PROBLEMS):
        p = api.post(f"/api/assessments/{aid}/problems", {
            "number": i + 1, "title": title, "statement": statement,
            "max_points": "10", "position": i + 1,
        })
        problems[code] = p
        api.post(f"/api/problems/{p['id']}/rubric", {
            "notes": f"Demo rubric for {code} (seed-demo-walkthrough)",
            "score_increment": "0.5",
            "criteria": [
                {"description": d, "points": pts, "partial_credit_notes": notes}
                for d, pts, notes in RUBRICS[code]
            ],
        })
        api.post(f"/api/problems/{p['id']}/solutions",
                 {"content": "\n".join(mdd.GOOD[code])})
    log(f"[1] created {len(problems)} problems, each with a rubric + reference solution")

    # Region setup mirrors the Identify path's finalize seeding (D66): id-regions
    # for the three header boxes, then the student_id+name rects copied into
    # mask_regions with page_scope 'all' so masking hides identity everywhere.
    boxes = {"student_id": mdd.BOXES[0], "name": mdd.BOXES[1], "problem_id": mdd.BOXES[2]}
    colors = {"student_id": "#2563eb", "name": "#16a34a", "problem_id": "#ea580c"}
    y, h = mdd.BOX_TOP, mdd.BOX_BOT - mdd.BOX_TOP
    api.put(f"/api/assessments/{aid}/id-regions", {"regions": [
        {"kind": kind, "x": x0, "y": y, "w": round(x1 - x0, 6), "h": h,
         "color": colors[kind], "padding": 0.01}
        for kind, (x0, x1, _label) in boxes.items()
    ]})
    api.put(f"/api/assessments/{aid}/mask-regions", {"regions": [
        {"page_scope": "all", "x": x0, "y": y, "w": round(x1 - x0, 6), "h": h,
         "color": colors[kind], "padding": 0.01}
        for kind, (x0, x1, _label) in boxes.items() if kind != "problem_id"
    ]})
    log("[1] id-regions saved; student_id+name mask regions derived (page_scope=all)")
    return aid, problems


def intake(api, aid):
    bodies = demo_answer_bodies()
    files = [(f"{sid}.pdf", student_pdf(sid, name, bodies)) for sid, name in mdd.STUDENTS]
    body, ctype = multipart_body(files)
    res = api.post(f"/api/assessments/{aid}/submissions", body, content_type=ctype)
    queued = [r for r in res["results"] if r["status"] == "queued"]
    if len(queued) != len(files):
        die(f"upload staging rejected files: {res['results']}")
    log(f"[2] uploaded {len(files)} per-student PDFs (all queued for ingest)")

    def uploads_done():
        rows = api.get(f"/api/assessments/{aid}/uploads")["uploads"]
        pending = [r for r in rows if r["status"] in ("pending", "queued")]
        bad = [r for r in rows if r["status"] not in ("pending", "queued", "ingested")]
        if bad:
            die(f"ingest failed for {len(bad)} uploads: {bad}")
        return (not pending, f"{len(rows) - len(pending)}/{len(rows)} ingested")

    poll("direct uploads to ingest", uploads_done, timeout=300)

    report = api.get(f"/api/assessments/{aid}/ingest/report")
    if report["quarantine"]:
        die(f"unexpected quarantine entries: {report['quarantine']}")
    demo_sids = {sid for sid, _ in mdd.STUDENTS}
    incomplete = [
        r for r in report["students"]
        if r["student_id"] in demo_sids and r["mapped_pages"] != r["expected_pages"]
    ]
    if incomplete:
        die(f"{len(incomplete)} demo students not mapped-complete: {incomplete}")
    log(f"[2] ingest report: all {len(demo_sids)} demo students MAPPED {len(mdd.PROBLEMS)}/{len(mdd.PROBLEMS)}, quarantine empty")


def masking(api, aid):
    res = api.post(f"/api/assessments/{aid}/masks/apply")
    log(f"[3] mask apply: enqueued={res['enqueued']} skipped={res['skipped']}")

    def masked_done():
        pages = api.get(f"/api/assessments/{aid}/masks/review")["pages"]
        errs = [p for p in pages if p.get("mask_error")]
        if errs:
            die(f"mask jobs failed on {len(errs)} pages: {errs[:3]}")
        left = [p for p in pages if not p["masked"]]
        return (not left, f"{len(pages) - len(left)}/{len(pages)} masked")

    poll("mask rendering", masked_done, timeout=600)
    res = api.post(f"/api/assessments/{aid}/masks/accept-pending")
    log(f"[3] masks rendered; accept-pending accepted {res['accepted']} pages")


def materialize(api, aid):
    res = api.post(f"/api/assessments/{aid}/materialize-answers")
    log(f"[4] materialized {res['created']} answer rows for the rest of the roster (they publish as skipped)")


def pick_flash_method(api):
    methods = [m for m in api.get("/api/methods")["methods"] if not m["archived"]]
    flash = [m for m in methods if "flash" in m["latest"]["config"].get("model", "").lower()]
    if not flash:
        die("no configured method with a 'flash' model — create one on the Methods page first")
    m = flash[0]
    cfg = m["latest"]["config"]
    log(f"[5] method: id={m['id']} {m['name']!r} model={cfg.get('model')} provider={cfg.get('provider')}")
    return m


def ensure_pricing(api, method):
    cfg = method["latest"]["config"]
    provider_name, model = cfg.get("provider"), cfg.get("model")
    providers = api.get("/api/providers")["providers"]
    prov = next((p for p in providers if p["name"] == provider_name), None)
    if prov is None:
        log(f"[5] provider {provider_name!r} has no llm_providers row — cost will be unknown")
        return
    rows = api.get(f"/api/providers/{prov['id']}/pricing")["pricing"]
    if any(r["model"] == model for r in rows):
        return
    if model not in KNOWN_PRICING_USD_PER_MTOK:
        log(f"[5] no pricing known for {model!r} — run cost will report 0 (NULL per-record)")
        return
    inp, outp = KNOWN_PRICING_USD_PER_MTOK[model]
    api.put(f"/api/providers/{prov['id']}/pricing", {
        "model": model, "input_usd_per_mtok": inp, "output_usd_per_mtok": outp,
    })
    log(f"[5] entered pricing for {model}: ${inp}/M in, ${outp}/M out (docs/MODELS.md)")


def run_ai(api, aid, method):
    # Resume-friendly: a completed assessment-scope run for this method is
    # reused rather than paying for a second one (--continue after --no-ai).
    for r in api.get(f"/api/runs?assessment_id={aid}")["runs"]:
        if r["status"] == "completed" and r["scope_kind"] == "assessment" \
                and r.get("method_name") == method["name"]:
            log(f"[5] reusing completed run {r['id']} "
                f"(cost=${r['cost_usd']} tokens={r['input_tokens']}in/{r['output_tokens']}out)")
            return r
    run = api.post("/api/runs", {
        "assessment_id": aid, "scope_kind": "assessment",
        "scope_id": aid, "method_id": method["id"],
    })
    rid = run["id"]
    log(f"[5] launched run {rid} (assessment scope)")

    retries = 0

    def run_done():
        nonlocal retries
        r = api.get(f"/api/runs/{rid}")["run"]
        state = f"status={r['status']} counts={r['counts']} cost=${r['cost_usd']}"
        if r["status"] in ("pending", "running", "paused"):
            return (False, state)
        if r["status"] in ("cancelled",):
            die(f"run {rid} was cancelled")
        failed = r["counts"].get("failed", 0)
        if failed and retries < 2:
            retries += 1
            log(f"[5] run {rid}: {failed} failed leaves — retry #{retries}")
            api.post(f"/api/runs/{rid}/retry-failed")
            return (False, state)
        if r["status"] == "failed" or failed:
            die(f"run {rid} ended with failures: {state}")
        return (True, r)

    r = poll(f"run {rid} to complete", run_done, timeout=3600, interval=10)
    log(f"[5] run {rid} completed: counts={r['counts']} "
        f"cost=${r['cost_usd']} tokens={r['input_tokens']}in/{r['output_tokens']}out")
    return r


def spot_check(api, rid):
    sc = api.get(f"/api/runs/{rid}/spot-check")
    todo = [s for s in sc["samples"] if not s.get("verdict")]
    for s in todo:
        api.post(f"/api/runs/{rid}/spot-check/{s['id']}",
                 {"verdict": "agree", "note": "seed-demo-walkthrough: checked against the page image"})
    state = api.get(f"/api/runs/{rid}/spot-check")["state"]
    log(f"[5] spot-check: {state['done']}/{state['total']} verdicts recorded")
    if state["total"] and state["done"] != state["total"]:
        die(f"spot-check gate still open: {state}")


def manual_fallback_for_blockers(api, aid, blockers):
    """A refusal/illegible leaf leaves official=NULL — file a fallback manual
    record (0 per criterion) so coverage closes; officials recompute makes it
    official only where the AI source left the answer undecided (0027)."""
    assessment = api.get(f"/api/assessments/{aid}")
    by_number = {p["number"]: p for p in assessment["problems"]}
    for b in blockers:
        if b["kind"] != "ungraded":
            die(f"unexpected publish blocker kind: {b}")
        problem = by_number[b["problem_number"]]
        rubric = api.get(f"/api/problems/{problem['id']}/rubric")["current"]
        api.post(f"/api/answers/{b['answer_id']}/records", {
            "rubric_version_id": rubric["id"],
            "comment": "No legible answer on the page — 0 (seed-demo-walkthrough fallback).",
            "scores": [
                {"criterion_id": c["id"], "score": "0",
                 "rationale": "no gradable content"}
                for c in rubric["criteria"]
            ],
        })
    log(f"[6] filed manual fallback records for {len(blockers)} ungraded answers")


def choose_final_source_and_publish(api, aid, run):
    api.put(f"/api/assessments/{aid}/final-source",
            {"kind": "method", "run_id": run["id"]})
    log(f"[6] final source pinned to completed run {run['id']}")

    pv = api.get(f"/api/assessments/{aid}/publish/preview")
    if pv["has_live_batch"]:
        log("[6] a live publish batch already exists — skipping publish")
        return
    if not pv["publishable"] and pv["blockers"]:
        manual_fallback_for_blockers(api, aid, pv["blockers"])
        pv = api.get(f"/api/assessments/{aid}/publish/preview")
    if not pv["publishable"]:
        die(f"publish preview not publishable: blocked={pv['blocked']} "
            f"not_ingested={pv['not_ingested']} blockers={pv['blockers'][:5]}")
    log(f"[6] coverage: graded={pv['graded']} no_submission={pv['no_submission']} "
        f"of total={pv['total_answers']}; skipped students={len(pv['skipped'] or [])}")

    # Exactly the UI's payload (PublishTab publish dialog defaults).
    res = api.post(f"/api/assessments/{aid}/publish", {
        "note": "Demo walkthrough publish (seed-demo-walkthrough.py)",
        "resend_all": False, "attachment": "none", "zip": False,
    })
    log(f"[6] published batch {res['batch_id']}: items={res['items_created']} "
        f"enqueued={res['enqueued']} skipped={res['skipped']}"
        if "items_created" in res else f"[6] published: {res}")

    def sends_done():
        batches = api.get(f"/api/assessments/{aid}/publish/batches")["batches"]
        live = next((b for b in batches if not b["superseded"]), None)
        if live is None:
            return (False, "no live batch yet")
        # `uncertain` is terminal but not success: the provider may have accepted
        # the message, so an operator must reconcile it before explicitly accepting
        # duplicate-delivery risk. Never poll it as though work were still moving.
        terminal = ("sent", "failed", "skipped", "uncertain")
        pending = [i for i in live["items"] if i["email_status"] not in terminal]
        failed = [i for i in live["items"] if i["email_status"] == "failed"]
        uncertain = [i for i in live["items"] if i["email_status"] == "uncertain"]
        if uncertain:
            die(f"{len(uncertain)} grade email outcomes are uncertain; reconcile provider delivery "
                "before any duplicate-risk resend")
        if not pending and failed:
            die(f"{len(failed)} grade emails failed to send: {failed[:3]}")
        return (not pending, f"{len(live['items']) - len(pending)}/{len(live['items'])} sent")

    poll("grade emails to send (file provider outbox)", sends_done, timeout=300)
    log(f"[6] all grade emails written to the outbox ({OUTBOX})")


# --- regrades ------------------------------------------------------------------------


def outbox_token_for(email_addr):
    """Newest outbox .eml addressed to email_addr carrying a regrade Reply-To."""
    if not OUTBOX.is_dir():
        return None
    for path in sorted(OUTBOX.glob("*.eml"), reverse=True):
        try:
            with open(path, "rb") as f:
                msg = BytesParser(policy=policy.default).parse(f, headersonly=True)
        except Exception:
            continue
        if (msg["To"] or "").strip().lower() != email_addr.lower():
            continue
        m = re.match(r"regrade\+(?P<tok>[^@]+)@", (msg["Reply-To"] or "").strip())
        if m:
            return m.group("tok")
    return None


def ensure_token(api, aid, student_sid, email_addr):
    """Token from the outbox, or resend that student's publish item (server
    mints Reply-To at send time — a restart that set the reply domain makes the
    resent copy carry one)."""
    tok = outbox_token_for(email_addr)
    if tok:
        return tok
    batches = api.get(f"/api/assessments/{aid}/publish/batches")["batches"]
    live = next((b for b in batches if not b["superseded"]), None)
    if live is None:
        die("no live publish batch — run the full seeder first")
    item = next((i for i in live["items"] if i["student_id"] == student_sid), None)
    if item is None:
        die(f"no publish item for student {student_sid} in live batch {live['id']}")
    api.post(f"/api/publish/items/{item['id']}/resend")
    log(f"    resent publish item {item['id']} to mint a regrade token…")

    def token_appears():
        t = outbox_token_for(email_addr)
        return (t is not None, "waiting for resent .eml with Reply-To")

    if poll("resent grade email with a Reply-To token", token_appears,
            timeout=60, fatal=False) is None:
        return None
    return outbox_token_for(email_addr)


def postmark_payload(from_email, from_name, token, subject, body):
    return {
        "From": f"{from_name} <{from_email}>",
        "FromFull": {"Email": from_email, "Name": from_name, "MailboxHash": token},
        "MailboxHash": token,
        "Subject": subject,
        "MessageID": str(uuid.uuid4()),
        "Date": time.strftime("%a, %d %b %Y %H:%M:%S +0800"),
        "TextBody": body,
        "StrippedTextReply": body,
        "Headers": [
            {"Name": "Received-SPF", "Value": "pass (demo.example: domain designates sender)"},
            {"Name": "Authentication-Results", "Value": "spf=pass; dkim=pass header.d=demo.example"},
        ],
    }


REGRADE_ONE = """Dear TAs,

I believe my greedy proof was under-credited.

<p2>
My exchange argument does swap o_i for g_i and I argue compatibility right
after the swap (third line of my answer). The comment says the induction was
incomplete, but both the base case and the step are on the page. Could you
re-check criterion 2?
</p2>

Thank you!"""

REGRADE_TWO = """Hello,

Two of my scores look inconsistent with the rubric.

<p1>
I did state the O(log n) bound and unrolled the recurrence to k = log2 n.
The deduction note says the worst case was missing, but my last line covers
the absent-element case with the probe count.
</p1>

<p3>
My recurrence includes the a[j] < a[i] condition — it may look like an index
typo, but j ranges over smaller indices only. Please look at the recurrence
line again before keeping the deduction.
</p3>

Thanks for your time."""


def file_regrades(api, base, aid):
    """Two regrade threads through the REAL inbound webhook (spec §4): one
    contesting one problem, one contesting two. Returns True when filed."""
    already = [r for r in api.get(f"/api/regrades?assessment={aid}&kind=filed")["regrades"]
               if r["status"] == "received"]
    if len(already) >= 2:
        log(f"[7] {len(already)} open regrade threads already filed — skipping")
        return True

    # Probe the webhook route BEFORE touching tokens: an unconfigured (or wrong)
    # secret 404s for ANY body, side-effect-free — and an unparseable {} probe
    # writes no row on a live secret either (ParseInbound rejects it first).
    status, _ = api.request("POST", f"/webhooks/email/inbound/{WEBHOOK_SECRET}", {})
    if status == 404:
        log("")
        log("[7] REGRADES NOT FILED — the inbound webhook is not configured in the")
        log("    running server (secret mismatch/unset: 404). scripts/dev-e2e.sh now")
        log("    defaults ADAMARKER_INBOUND_WEBHOOK_SECRET + ADAMARKER_EMAIL_REPLY_DOMAIN;")
        log("    restart the dev server and re-run:")
        log("        uv run --with reportlab python scripts/seed-demo-walkthrough.py --regrades-only")
        return False

    picks = [
        (mdd.STUDENTS[2], "Re: your Demo Exam — completed results", REGRADE_ONE),
        (mdd.STUDENTS[6], "Re: Demo Exam — completed — two problems", REGRADE_TWO),
    ]
    payloads = []
    for (sid, name), subject, body in picks:
        email_addr = f"{sid.lower()}@demo.example"
        token = ensure_token(api, aid, sid, email_addr)
        if token is None:
            log("")
            log("[7] REGRADES NOT FILED — grade emails carry no Reply-To token, so the")
            log("    server was started without ADAMARKER_EMAIL_REPLY_DOMAIN. Restart the")
            log("    dev server (scripts/dev-e2e.sh now defaults it) and re-run:")
            log("        uv run --with reportlab python scripts/seed-demo-walkthrough.py --regrades-only")
            return False
        payloads.append(postmark_payload(email_addr, name, token, subject, body))

    for payload in payloads:
        status, resp = api.request("POST", f"/webhooks/email/inbound/{WEBHOOK_SECRET}", payload)
        if status == 404:
            log("[7] REGRADES NOT FILED — webhook 404 (secret rejected mid-flow).")
            return False
        if status == 400:
            log("")
            log("[7] REGRADES NOT FILED — the webhook answered 400 (invalid inbound payload).")
            log("    The configured email provider cannot parse inbound mail: the dev 'file'")
            log("    provider's ParseInbound always errors (internal/email/file.go), so even a")
            log("    correct secret cannot file threads. Filing regrades in dev needs a small")
            log("    code change (file provider delegating inbound parsing to the Postmark")
            log("    format) — outside this seeder's scope. Everything else is seeded.")
            return False
        if status == 503:
            log("[7] REGRADES NOT FILED — no email provider wired (503).")
            return False
        if status != 200:
            die(f"webhook POST -> {status}: {resp}")

    rows = api.get(f"/api/regrades?assessment={aid}&kind=filed")["regrades"]
    open_rows = [r for r in rows if r["status"] == "received"]
    if len(open_rows) < 2:
        die(f"expected 2 open filed regrade requests, found {len(open_rows)}: {rows}")
    log(f"[7] filed {len(open_rows)} regrade threads through the inbound webhook "
        f"(1 single-problem, 1 two-problem)")
    return True


# --- summary / assertions ------------------------------------------------------------


def summarize(api, aid):
    pv = api.get(f"/api/assessments/{aid}/publish/preview")
    runs = api.get(f"/api/runs?assessment_id={aid}")["runs"]
    completed = [r for r in runs if r["status"] == "completed"]
    regrades = api.get(f"/api/regrades?assessment={aid}&kind=filed")["regrades"]
    open_regrades = [r for r in regrades if r["status"] == "received"]

    log("")
    log("=== Demo Exam — completed: state ===")
    log(f"  assessment          id={aid}  {ASSESSMENT_NAME!r}")
    log(f"  coverage            graded={pv['graded']} no_submission={pv['no_submission']} "
        f"blocked={pv['blocked']} not_ingested={pv['not_ingested']} total={pv['total_answers']}")
    log(f"  live publish batch  {pv['has_live_batch']}")
    if completed:
        r = completed[0]
        log(f"  latest AI run       id={r['id']} method={r.get('method_name', '?')} "
            f"cost=${r['cost_usd']} tokens={r['input_tokens']}in/{r['output_tokens']}out")
    log(f"  open regrades       {len(open_regrades)} filed (of {len(regrades)} filed rows)")
    log("====================================")
    return pv, completed, open_regrades


def assert_end_state(pv, expect_regrades, open_regrades):
    errs = []
    if pv["blocked"] or pv["not_ingested"]:
        errs.append(f"coverage not clean: blocked={pv['blocked']} not_ingested={pv['not_ingested']}")
    if pv["graded"] + pv["no_submission"] != pv["total_answers"]:
        errs.append("graded + no_submission != total_answers")
    if not pv["has_live_batch"]:
        errs.append("no live publish batch")
    if expect_regrades and len(open_regrades) < 2:
        errs.append(f"expected 2 open regrades, found {len(open_regrades)}")
    if errs:
        die("end-state assertions failed: " + "; ".join(errs))
    log("end-state assertions passed" + ("" if expect_regrades else " (regrades pending webhook)"))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--base", default=DEFAULT_BASE, help="server base URL")
    ap.add_argument("--no-ai", action="store_true",
                    help="stop before the AI grading run (steps 1-4 only)")
    ap.add_argument("--continue", dest="resume", action="store_true",
                    help="resume steps 5-7 on the existing assessment")
    ap.add_argument("--regrades-only", action="store_true",
                    help="only file the regrade threads against the existing assessment")
    args = ap.parse_args()

    api = API(args.base)
    api.post("/auth/dev-login", {"email": ADMIN_EMAIL})
    me = api.get("/api/me")["user"]
    log(f"signed in as user {me['id']} (role={me['role']}) on {args.base}")

    existing = find_assessment(api, ASSESSMENT_NAME)

    if args.regrades_only:
        if existing is None:
            die(f"{ASSESSMENT_NAME!r} does not exist yet — run without --regrades-only first")
        aid = existing["id"]
        filed = file_regrades(api, args.base, aid)
        pv, _completed, open_regrades = summarize(api, aid)
        assert_end_state(pv, filed, open_regrades)
        return

    if existing is not None and not args.resume:
        log(f"{ASSESSMENT_NAME!r} already exists (id={existing['id']}) — idempotent skip, printing state:")
        summarize(api, existing["id"])
        return

    if args.resume:
        if existing is None:
            die(f"--continue: {ASSESSMENT_NAME!r} does not exist yet — run without flags first")
        aid = existing["id"]
        log(f"--continue: resuming steps 5-7 on assessment {aid}")
    else:
        aid, _problems = create_structure(api)
        intake(api, aid)
        masking(api, aid)
        materialize(api, aid)

    method = pick_flash_method(api)
    ensure_pricing(api, method)
    if args.no_ai:
        log("")
        log("--no-ai: stopping before the AI run. To finish, re-run with --continue")
        log("(launches the run, records spot-check verdicts, sets the final source,")
        log("publishes, and files the demo regrade threads).")
        return

    run = run_ai(api, aid, method)
    spot_check(api, run["id"])
    choose_final_source_and_publish(api, aid, run)
    filed = file_regrades(api, args.base, aid)

    pv, _completed, open_regrades = summarize(api, aid)
    assert_end_state(pv, filed, open_regrades)


if __name__ == "__main__":
    main()
