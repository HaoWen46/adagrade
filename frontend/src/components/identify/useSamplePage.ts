// Sample-page lookup shared by the ID-region editor: falls back to any answer
// page the way MaskingTab's useSamplePage does. The scan-file-page waterfall is
// gone with the file-level pipeline — Task 11 restores a page-level equivalent
// once scan_pages rendering exists.

import { useRef } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../../lib/api";
import type { AnswerResponse, ProblemStudentRow, ProblemSummary } from "../../lib/types";

export function useSamplePage(assessmentId: string): { pageUrl?: string; pending: boolean } {
  // Latch: once a pageUrl is chosen for a given assessment, keep it until the
  // assessment changes, so the ID-region editor's sample image never swaps out
  // from under the user mid-session.
  const latch = useRef<{ assessmentId: string; pageUrl: string } | null>(null);
  if (latch.current && latch.current.assessmentId !== assessmentId) {
    latch.current = null;
  }

  const summary = useQuery({
    queryKey: ["problem-summaries", assessmentId],
    queryFn: () =>
      api.get<{ problems: ProblemSummary[] }>(`/api/assessments/${assessmentId}/problems/summary`),
  });
  const problemId = summary.data?.problems.find((p) => p.with_pages > 0)?.problem_id;

  const students = useQuery({
    queryKey: ["problem-students", problemId],
    queryFn: () => api.get<{ students: ProblemStudentRow[] }>(`/api/problems/${problemId}/students`),
    enabled: problemId !== undefined,
  });
  const answerId = students.data?.students.find((s) => s.page_count > 0)?.answer_id;

  const answer = useQuery({
    queryKey: ["answer", String(answerId)],
    queryFn: () => api.get<AnswerResponse>(`/api/answers/${answerId}`),
    enabled: answerId !== undefined,
  });

  // Already latched for this assessment: keep returning the same URL regardless
  // of what the polls resolve to now, so the editor's sample image never swaps
  // mid-session.
  if (latch.current) {
    return { pageUrl: latch.current.pageUrl, pending: false };
  }

  const pending =
    summary.isPending ||
    (problemId !== undefined && students.isPending) ||
    (answerId !== undefined && answer.isPending);

  const pageId = answer.data?.pages[0]?.id;
  if (pageId !== undefined) {
    const pageUrl = `/api/answer-pages/${pageId}/image`;
    latch.current = { assessmentId, pageUrl };
    return { pageUrl, pending };
  }
  return { pageUrl: undefined, pending };
}
