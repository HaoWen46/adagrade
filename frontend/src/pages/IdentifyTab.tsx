// Identify tab: scan intake + student identification (page-level design spec
// 2026-07-04). Task 11 restored the id-region editor, page-level upload, and the
// batch list; Task 12 added the assignment matrix, the orphan queue, and parked-page
// conflict/duplicate resolution; Task 13 adds assessment-wide finalize.

import { useQuery } from "@tanstack/react-query";
import { api } from "../lib/api";
import { BatchListCard } from "../components/identify/BatchListCard";
import { DiscardedCard } from "../components/identify/DiscardedCard";
import { FinalizeCard } from "../components/identify/FinalizeCard";
import { IDRegionCard } from "../components/identify/IDRegionCard";
import { MatrixCard } from "../components/identify/MatrixCard";
import { OrphanQueue } from "../components/identify/OrphanQueue";
import { ParkedCard } from "../components/identify/ParkedCard";
import { UploadCard } from "../components/identify/UploadCard";
import { WorkflowNotice } from "../components/WorkflowNotice";
import { useWorkflowWarnings, warningView } from "../lib/warnings";

export function IdentifyTab({ assessmentId }: { assessmentId: string }) {
  // duplicate_student_names (roster-lifecycle plan 2026-07-10): same-named students'
  // pages can never be auto-attributed by name, so Identify — where the manual
  // confirmation happens — carries the standing info notice. No fix-it link: it would
  // point right back here.
  const warnings = useWorkflowWarnings(assessmentId);
  const dupNames = (warnings.data?.warnings ?? []).find(
    (w) => w.code === "duplicate_student_names",
  );
  const dupNamesView = dupNames ? warningView(dupNames, assessmentId) : null;

  // Local-OCR absence is loud (privacy audit 2026-07-12): masking exists to
  // keep student identity off cloud providers, so when the on-machine OCR rung
  // isn't installed the tab says so up front — same queryKey as UploadCard, so
  // react-query dedupes the fetch. Only shown once the answer is known (no
  // flash while loading).
  const identifyStatus = useQuery({
    queryKey: ["identify-status"],
    queryFn: () => api.get<{ local_ocr_available: boolean }>("/api/identify/status"),
  });
  const localOCRMissing = identifyStatus.data?.local_ocr_available === false;

  return (
    <div className="space-y-4">
      <p className="text-xs text-neutral-400">
        Have one PDF per student instead of a scanner pile? Use the{" "}
        <a href="?tab=submissions" className="font-medium underline">
          Submissions tab
        </a>
        .
      </p>
      {dupNamesView && (
        <WorkflowNotice tone={dupNamesView.tone}>{dupNamesView.message}</WorkflowNotice>
      )}
      {localOCRMissing && (
        <WorkflowNotice tone="warning">
          Local OCR isn&apos;t installed on this server — if you enable the cloud step below,
          ID/name crops from every page will go to the cloud AI provider. Run{" "}
          <code className="font-mono">make ocr-models</code> and set{" "}
          <code className="font-mono">ADAMARKER_OCR_MODEL</code> to keep identification on this
          machine.
        </WorkflowNotice>
      )}
      <IDRegionCard assessmentId={assessmentId} />
      <UploadCard assessmentId={assessmentId} />
      <BatchListCard assessmentId={assessmentId} />
      <MatrixCard assessmentId={assessmentId} />
      <OrphanQueue assessmentId={assessmentId} />
      <ParkedCard assessmentId={assessmentId} />
      <DiscardedCard assessmentId={assessmentId} />
      <FinalizeCard assessmentId={assessmentId} />
    </div>
  );
}
