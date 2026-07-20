#!/usr/bin/env python3
"""Generate ADA-Marker demo data (spec 2026-07-04-guide-page-demo-data-design.md).

Run via: uv run --with reportlab python scripts/make-demo-data.py  (or `make demo-data`)

Outputs (deterministic — fixed seed; commit the results):
  data/demo/demo-roster.csv     10 synthetic students (never real people)
  data/demo/demo-exam.pdf       answer-sheet template page + 4 problem statements
  data/demo/demo-scan-pile.pdf  40 shuffled pages, filled header boxes, mixed answer quality

The original three files above consume random.Random(SEED=46) in a fixed order
and must stay byte-identical across regenerations. Everything below is NEW and
uses a SEPARATE random.Random(SEED2=47) so adding artifacts never shifts them:
  data/demo/demo-roster-v2.csv        week-3 add/drop re-import: drops B11902009+
                                      B11902010, adds B11902011+B11902012, one
                                      email fix + one name-spelling fix — the
                                      Students import diff demo
  data/demo/demo-roster-mistakes.csv  rejected import: one mistake per concept
                                      (duplicate email, normalize-colliding ids,
                                      invalid email, empty name) — per-line errors
  data/demo/demo-roster-big5.csv      demo-roster.csv rows encoded in Big5 — the
                                      "not valid UTF-8, use Excel CSV UTF-8" demo
  data/demo/demo-scan-pile-messy.pdf  12-page intake edge-path pile: 6 normal,
                                      2 duplicates (parked/conflict), 2 unreadable
                                      ID boxes + 1 unknown id (orphans), 1 blank
  data/demo/demo-submissions/<id>.pdf per-student 4-page PDFs (problem order,
                                      same SEED=46 answer bodies as the pile) for
                                      manually demoing the Submissions drag-drop
"""

import csv
import random
from pathlib import Path

from reportlab.lib.pagesizes import A4
from reportlab.pdfbase import pdfmetrics
from reportlab.pdfbase.cidfonts import UnicodeCIDFont
from reportlab.pdfgen import canvas

SEED = 46
SEED2 = 47  # new artifacts only — never feeds the SEED=46 outputs
OUT = Path(__file__).resolve().parent.parent / "data" / "demo"
W, H = A4

# CID font ships with reportlab — renders CJK without bundling a TTF.
CJK = "STSong-Light"
pdfmetrics.registerFont(UnicodeCIDFont(CJK))

# Synthetic roster: invented names, demo emails. Never real people.
STUDENTS = [
    ("B11902001", "丁一心"), ("B11902002", "卜書言"), ("B11902003", "石可久"),
    ("B11902004", "白凡星"), ("B11902005", "皮向文"), ("B11902006", "田禾多"),
    ("B11902007", "羊念真"), ("B11902008", "米樂水"), ("B11902009", "貝同光"),
    ("B11902010", "車以方"),
]

PROBLEMS = [
    ("Q1", "Binary search complexity",
     "State the worst-case running time of binary search on a sorted array of n "
     "elements and prove your bound by analysing the recurrence T(n) = T(n/2) + O(1)."),
    ("Q2", "Greedy interval scheduling",
     "You are given n intervals. Prove that repeatedly choosing the compatible "
     "interval with the earliest finish time yields a maximum-size set of "
     "non-overlapping intervals (exchange argument expected)."),
    ("Q3", "Longest increasing subsequence",
     "Give a dynamic-programming algorithm for the longest increasing subsequence "
     "of a sequence of n numbers. Define your subproblems, give the recurrence, "
     "and state the running time. An O(n log n) refinement earns full marks."),
    ("Q4", "BFS shortest paths",
     "Prove or disprove: in an unweighted graph, breadth-first search from s "
     "computes a shortest path from s to every reachable vertex. Justify with an "
     "invariant over BFS layers."),
]

# Header-box geometry, normalized to page size; y measured from the TOP edge to
# match the app's region coordinates (drawing code converts to PDF bottom-up).
BOXES = [(0.05, 0.30, "Student ID"), (0.35, 0.60, "Name"), (0.65, 0.90, "Problem")]
BOX_TOP, BOX_BOT = 0.02, 0.08

GOOD = {
    "Q1": ["T(n) = T(n/2) + O(1). Unrolling: after k halvings the array has n/2^k",
           "elements; we stop when n/2^k = 1, i.e. k = log2 n. Each level does O(1)",
           "work, so T(n) = O(log n). Worst case: the element is absent, and we",
           "probe until the range is empty — still log2 n + 1 probes."],
    "Q2": ["Exchange argument: let G = g1..gk be greedy's picks (by finish time) and",
           "OPT = o1..om an optimal solution, both sorted. By induction f(gi) <= f(oi):",
           "greedy picks the earliest-finishing compatible interval, so we can swap",
           "oi for gi without breaking compatibility. Hence k >= m, so greedy is optimal."],
    "Q3": ["Let L[i] = length of the LIS ending exactly at a[i].",
           "L[i] = 1 + max{ L[j] : j < i, a[j] < a[i] } (or 1 if none). Answer max L[i].",
           "Two nested loops give O(n^2). Refinement: keep tails[k] = smallest tail of",
           "an increasing subsequence of length k+1; binary-search each element: O(n log n)."],
    "Q4": ["True. Invariant: when BFS finishes layer d, every vertex at distance d has",
           "been discovered with dist = d. Base: layer 0 is {s}. Step: any v at distance",
           "d+1 has a neighbour u at distance d; when u is dequeued v is discovered with",
           "dist d+1, and no earlier layer can reach v or its distance would be < d+1."],
}

WRONG = {
    "Q1": ["Binary search is O(n) in the worst case because if the element is missing",
           "you may have to look at every cell to be sure. The recurrence just shows",
           "the best case is faster."],
    "Q2": ["Pick the shortest interval first — the shortest one blocks the fewest",
           "others, so by induction the shortest-first greedy is optimal. QED."],
    "Q3": ["Sort the array, then the LIS is the whole sorted array, so the answer is",
           "always n and this runs in O(n log n)."],
    "Q4": ["False: BFS can take a longer path if it enqueues vertices in a bad order,",
           "e.g. a diamond graph where the right branch is explored first."],
}

SHORT = {"Q1": ["O(log n)?"], "Q2": ["Greedy works. Exchange argument."],
         "Q3": ["DP, O(n^2)."], "Q4": ["True by induction."]}

GARBLED = [
    ["(pencil smudge) ... midterm of the other course ... (crossed out)",
     "see attached sheet (no sheet attached)"],
    ["I ran out of time — please see my Q2 answer which also covers this.",
     "(arrow pointing off the page)"],
]


def draw_header(c, sid="", name="", prob="", labels=False):
    y0 = H * (1 - BOX_BOT)
    y1 = H * (1 - BOX_TOP)
    values = [sid, name, prob]
    for (x0f, x1f, label), val in zip(BOXES, values):
        x0, x1 = W * x0f, W * x1f
        c.rect(x0, y0, x1 - x0, y1 - y0)
        if labels:
            c.setFont("Helvetica", 7)
            c.drawString(x0 + 4, y1 - 10, label)
        if val:
            c.setFont(CJK, 15)
            c.drawString(x0 + 10, y0 + 10, val)


def wrap_lines(c, lines, x, y, leading=18, font=("Helvetica", 11)):
    c.setFont(*font)
    for ln in lines:
        c.drawString(x, y, ln)
        y -= leading


def make_exam():
    c = canvas.Canvas(str(OUT / "demo-exam.pdf"), pagesize=A4, invariant=1)
    # Page 1: the answer-sheet template — empty boxes; use as the region-editor
    # template image (positions match the Guide walkthrough).
    draw_header(c, labels=True)
    c.setFont("Helvetica-Bold", 18)
    c.drawCentredString(W / 2, H * 0.82, "ADA Demo Exam — Answer Sheet")
    wrap_lines(c, [
        "Write in the three boxes at the top of EVERY page:",
        "  left: your student ID (e.g. B11902001)",
        "  middle: your name",
        "  right: the problem number, prefixed (Q1, Q2, Q3, Q4)",
        "Use one page per problem. Each problem is worth 10 points.",
    ], W * 0.12, H * 0.72, leading=22, font=("Helvetica", 12))
    c.showPage()
    for code, title, body in PROBLEMS:
        c.setFont("Helvetica-Bold", 14)
        c.drawString(W * 0.1, H * 0.9, f"{code}. {title}  (10 points)")
        words, line, lines = body.split(), "", []
        for w_ in words:
            if len(line) + len(w_) > 78:
                lines.append(line)
                line = w_
            else:
                line = (line + " " + w_).strip()
        lines.append(line)
        wrap_lines(c, lines, W * 0.1, H * 0.84)
        c.showPage()
    c.save()


def answer_for(rng, code):
    roll = rng.random()
    if roll < 0.50:
        return GOOD[code]
    if roll < 0.65:
        return WRONG[code]
    if roll < 0.80:
        return SHORT[code]
    if roll < 0.90:
        return []  # blank: header only
    return rng.choice(GARBLED)


def make_pile(rng):
    """Writes the 40-page pile; returns {(sid, code): body_lines} so the
    per-student submission PDFs reuse the exact same answers without consuming
    any extra rng draws (seed usage order must stay byte-identical)."""
    pages = [(sid, name, code) for sid, name in STUDENTS for code, _, _ in PROBLEMS]
    rng.shuffle(pages)
    c = canvas.Canvas(str(OUT / "demo-scan-pile.pdf"), pagesize=A4, invariant=1)
    bodies = {}
    for sid, name, code in pages:
        draw_header(c, sid, name, code)
        body = answer_for(rng, code)
        bodies[(sid, code)] = body
        wrap_lines(c, body, W * 0.1, H * 0.8, leading=22, font=("Helvetica", 12))
        c.showPage()
    c.save()
    return bodies


def write_roster(path, rows, encoding="utf-8"):
    with open(path, "w", newline="", encoding=encoding) as f:
        w = csv.writer(f)
        w.writerow(["student_id", "name", "email"])
        w.writerows(rows)


def roster_rows():
    return [(sid, name, f"{sid.lower()}@demo.example") for sid, name in STUDENTS]


def make_roster():
    write_roster(OUT / "demo-roster.csv", roster_rows())


# --- new artifacts (SEED2 only) --------------------------------------------------


def make_roster_v2():
    """Week-3 add/drop re-import against a demo-roster.csv baseline. The import
    diff shows: missing_active = [B11902009, B11902010] (dropped the course),
    two added students, email_changed = 1 (B11902003), name_changed = 1
    (B11902007 真 → 眞 spelling fix)."""
    rows = []
    for sid, name, email in roster_rows():
        if sid in ("B11902009", "B11902010"):
            continue  # dropped in add/drop — absent from the week-3 CSV
        if sid == "B11902003":
            email = "b11902003@student.demo.example"  # email fix
        if sid == "B11902007":
            name = "羊念眞"  # name-spelling fix
        rows.append((sid, name, email))
    rows.append(("B11902011", "宋竹青", "b11902011@demo.example"))  # late adds
    rows.append(("B11902012", "林一帆", "b11902012@demo.example"))
    write_roster(OUT / "demo-roster-v2.csv", rows)


def make_roster_mistakes():
    """A roster the importer REJECTS whole (D13) with per-line errors, one
    mistake per concept. Line numbers are 1-based including the header:
      line 5: id collides with line 4 under studentid.Normalize (case/width)
      line 7: shares line 6's email
      line 8: invalid email (no @)
      line 9: empty name"""
    write_roster(OUT / "demo-roster-mistakes.csv", [
        ("B11902001", "丁一心", "b11902001@demo.example"),  # fine
        ("B11902002", "卜書言", "b11902002@demo.example"),  # fine
        ("b11902003", "石可久", "b11902003@demo.example"),  # fine on its own…
        ("Ｂ１１９０２００３", "石可久", "b11902003.retype@demo.example"),  # …collides after NFKC+case fold
        ("B11902004", "白凡星", "shared@demo.example"),  # fine on its own…
        ("B11902005", "皮向文", "shared@demo.example"),  # …duplicate email
        ("B11902006", "田禾多", "b11902006.demo.example"),  # invalid email (no @)
        ("B11902007", "", "b11902007@demo.example"),  # empty name
    ])


def make_roster_big5():
    """Same rows as demo-roster.csv, saved the way Chinese-locale Excel does by
    default (Big5/CP950) — the importer rejects it whole with the 'use Save As →
    CSV UTF-8' message."""
    write_roster(OUT / "demo-roster-big5.csv", roster_rows(), encoding="big5")


def scribble(c):
    """Illegible pen scribble over the student-ID header box (orphan path)."""
    x0f, x1f, _ = BOXES[0]
    x0, x1 = W * x0f + 8, W * x1f - 8
    y0, y1 = H * (1 - BOX_BOT) + 6, H * (1 - BOX_TOP) - 6
    c.saveState()
    c.setLineWidth(2)
    for i in range(9):
        t = i / 8
        c.line(x0 + (x1 - x0) * t, y0 if i % 2 else y1,
               x0 + (x1 - x0) * min(t + 0.18, 1.0), y1 if i % 2 else y0)
    c.restoreState()


def make_messy_pile(rng):
    """12 pages exercising the intake edge paths (Identify tab):
      6 normal pages           — clean header boxes, mixed answer tiers
      2 duplicate pages        — same student+problem as two normal pages but
                                 DIFFERENT answer text → parked/conflict path
      2 unreadable-ID pages    — one empty ID box, one pen-scribbled ID box → orphans
      1 unknown-student page   — B99999999 not on any roster → orphan
      1 blank page             — empty header boxes, no body
    Page tiers are fixed (not rng-rolled) so the duplicates provably differ from
    their originals; only the page order comes from the SEED2 rng."""
    pages = [
        # (sid, name, problem, body, scribbled_id)
        ("B11902001", "丁一心", "Q1", GOOD["Q1"], False),   # normal
        ("B11902002", "卜書言", "Q2", WRONG["Q2"], False),  # normal
        ("B11902003", "石可久", "Q3", GOOD["Q3"], False),   # normal
        ("B11902004", "白凡星", "Q4", SHORT["Q4"], False),  # normal
        ("B11902005", "皮向文", "Q1", GOOD["Q1"], False),   # normal
        ("B11902006", "田禾多", "Q2", GOOD["Q2"], False),   # normal
        ("B11902001", "丁一心", "Q1", WRONG["Q1"], False),  # duplicate of page 1, different answer
        ("B11902002", "卜書言", "Q2", SHORT["Q2"], False),  # duplicate of page 2, different answer
        ("", "米樂水", "Q3", SHORT["Q3"], False),           # empty ID box → orphan
        ("", "皮向文", "Q1", GARBLED[0], True),             # scribbled-illegible ID box → orphan
        ("B99999999", "何許人", "Q4", GOOD["Q4"], False),   # unknown student id → orphan
        (None, None, None, [], False),                      # blank page, empty header boxes
    ]
    rng.shuffle(pages)
    c = canvas.Canvas(str(OUT / "demo-scan-pile-messy.pdf"), pagesize=A4, invariant=1)
    for sid, name, code, body, scribbled in pages:
        draw_header(c, sid or "", name or "", code or "")
        if scribbled:
            scribble(c)
        wrap_lines(c, body, W * 0.1, H * 0.8, leading=22, font=("Helvetica", 12))
        c.showPage()
    c.save()


def make_submissions(bodies):
    """One 4-page PDF per roster student, pages in problem order (the
    Submissions path maps page i → problem i positionally), carrying the same
    SEED=46 answer bodies as the committed scan pile — so a manual drag-drop
    demo matches what seed-demo-walkthrough.py uploads."""
    subdir = OUT / "demo-submissions"
    subdir.mkdir(parents=True, exist_ok=True)
    for sid, name in STUDENTS:
        c = canvas.Canvas(str(subdir / f"{sid}.pdf"), pagesize=A4, invariant=1)
        for code, _title, _body in PROBLEMS:
            draw_header(c, sid, name, code)
            wrap_lines(c, bodies[(sid, code)], W * 0.1, H * 0.8,
                       leading=22, font=("Helvetica", 12))
            c.showPage()
        c.save()


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    # SEED=46 outputs first, in the original order — byte-identical forever.
    rng = random.Random(SEED)
    make_roster()
    make_exam()
    bodies = make_pile(rng)
    # New artifacts on a separate rng so the three outputs above never shift.
    rng2 = random.Random(SEED2)
    make_roster_v2()
    make_roster_mistakes()
    make_roster_big5()
    make_messy_pile(rng2)
    make_submissions(bodies)
    print(f"wrote demo data to {OUT}")


if __name__ == "__main__":
    main()
