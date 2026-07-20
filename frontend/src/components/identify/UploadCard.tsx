// Batch upload card: PDFs (multi-select) XOR one zip archive, plus OCR controls.
// Page-level scan intake (design spec 2026-07-04) has no problem-scope field — every
// page carries its own identity via the id-regions crop, so upload is a flat batch of
// source files, not scoped to one problem.
//
// Cloud identification has no default provider (the server 400s ocr_enabled
// with an empty provider), so when OCR is on the provider choice is explicit:
// the first enabled provider is preselected and Upload stays disabled until
// one is chosen.
//
// The cloud step itself is opt-IN (privacy audit 2026-07-12): the checkbox
// defaults to unchecked, matching the server's absent-field default, and the
// consequence of the current setting — local-only vs. fully manual — is
// spelled out right at the checkbox via GET /api/identify/status.

import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api, apiUpload } from "../../lib/api";
import type { CreateScanBatchResponse, Provider } from "../../lib/types";
import { HelpTip } from "../HelpTip";
import { Button, Card, Field, Select } from "../ui";

export function UploadCard({ assessmentId }: { assessmentId: string }) {
  const queryClient = useQueryClient();
  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: () => api.get<{ providers: Provider[] }>("/api/providers"),
  });
  // Whether this server has the on-machine OCR rung installed — drives the
  // consequence copy under the cloud checkbox (IdentifyTab shares the key, so
  // react-query dedupes the fetch).
  const identifyStatus = useQuery({
    queryKey: ["identify-status"],
    queryFn: () => api.get<{ local_ocr_available: boolean }>("/api/identify/status"),
  });
  const localOCR = identifyStatus.data?.local_ocr_available === true;

  const [mode, setMode] = useState<"files" | "zip">("files");
  const [files, setFiles] = useState<File[]>([]);
  const [zipFile, setZipFile] = useState<File | null>(null);
  const [ocrEnabled, setOcrEnabled] = useState(false);
  const [provider, setProvider] = useState("");
  const [model, setModel] = useState("");

  const enabledProviders = (providersQuery.data?.providers ?? []).filter((p) => p.enabled);
  // No "(default)" fallback exists server-side: derive the effective provider
  // as the explicit choice, else the first enabled one (preselection).
  const effectiveProvider = provider !== "" ? provider : (enabledProviders[0]?.name ?? "");
  const providerModels = enabledProviders.find((p) => p.name === effectiveProvider)?.models ?? [];
  const providerMissing = ocrEnabled && effectiveProvider === "";

  const upload = useMutation({
    mutationFn: () => {
      const form = new FormData();
      if (mode === "zip" && zipFile) {
        form.append("zip", zipFile);
      } else {
        for (const f of files) form.append("files", f);
      }
      form.append("ocr_enabled", ocrEnabled ? "1" : "0");
      if (ocrEnabled && effectiveProvider !== "") form.append("ocr_provider", effectiveProvider);
      if (ocrEnabled && model.trim() !== "") form.append("ocr_model", model.trim());
      return apiUpload<CreateScanBatchResponse>(
        `/api/assessments/${assessmentId}/scan-batches`,
        form,
      );
    },
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["scan-batches", assessmentId] }),
        queryClient.invalidateQueries({ queryKey: ["scan-pages", assessmentId] }),
      ]);
      setFiles([]);
      setZipFile(null);
    },
  });

  const regionsIncomplete =
    upload.error instanceof ApiError &&
    typeof upload.error.details === "object" &&
    upload.error.details !== null &&
    (upload.error.details as { regions_incomplete?: boolean }).regions_incomplete === true;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const hasInput = mode === "zip" ? zipFile !== null : files.length > 0;
    if (hasInput && !providerMissing) upload.mutate();
  };

  return (
    <Card title="Upload scans">
      <form className="space-y-3" onSubmit={submit}>
        <div className="flex items-center gap-4 text-sm">
          <label className="flex items-center gap-1.5">
            <input
              type="radio"
              name="upload-mode"
              checked={mode === "files"}
              onChange={() => setMode("files")}
              className="accent-indigo-600"
            />
            Individual PDFs
          </label>
          <label className="flex items-center gap-1.5">
            <input
              type="radio"
              name="upload-mode"
              checked={mode === "zip"}
              onChange={() => setMode("zip")}
              className="accent-indigo-600"
            />
            Zip archive
          </label>
        </div>

        {mode === "files" ? (
          <input
            type="file"
            multiple
            accept=".pdf,application/pdf"
            aria-label="Select PDF files"
            onChange={(e) => setFiles(Array.from(e.target.files ?? []))}
            className="w-full text-sm text-neutral-600 file:mr-3 file:rounded-md file:border file:border-solid file:border-neutral-300 file:bg-white file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-neutral-800 hover:file:bg-neutral-50"
          />
        ) : (
          <input
            type="file"
            accept=".zip,application/zip"
            aria-label="Select zip archive"
            onChange={(e) => setZipFile(e.target.files?.[0] ?? null)}
            className="w-full text-sm text-neutral-600 file:mr-3 file:rounded-md file:border file:border-solid file:border-neutral-300 file:bg-white file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-neutral-800 hover:file:bg-neutral-50"
          />
        )}

        <div className="grid grid-cols-2 gap-3">
          <Field label="OCR provider">
            <Select
              value={effectiveProvider}
              onChange={(e) => {
                setProvider(e.target.value);
                setModel("");
              }}
              disabled={!ocrEnabled}
            >
              {effectiveProvider === "" && (
                <option value="" disabled>
                  {providersQuery.isPending ? "Loading providers…" : "No providers configured"}
                </option>
              )}
              {enabledProviders.map((p) => (
                <option key={p.id} value={p.name}>
                  {p.name}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="OCR model">
            <Select
              value={model}
              onChange={(e) => setModel(e.target.value)}
              disabled={!ocrEnabled || effectiveProvider === ""}
            >
              <option value="">(default)</option>
              {providerModels.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </Select>
          </Field>
        </div>

        {providerMissing && !providersQuery.isPending && (
          <p className="text-xs text-amber-700">
            Cloud identification needs a provider and none is enabled — add one under Providers,
            or untick the cloud step below.
          </p>
        )}

        <label className="flex items-center gap-1.5 text-sm text-neutral-600">
          <input
            type="checkbox"
            checked={ocrEnabled}
            onChange={(e) => setOcrEnabled(e.target.checked)}
            className="size-3.5 accent-indigo-600"
          />
          Send unmatched IDs to the cloud AI model{" "}
          <HelpTip title="Cloud identification">
            <p>
              Identification tries filename matching first, then (if this server has it set up) a
              small local OCR model that never leaves the machine, then this cloud step, then a
              human review.
            </p>
            <p>
              This toggle controls only the <strong>cloud</strong> step: whether an ID crop that
              the earlier steps couldn&apos;t read gets sent to an AI provider over the internet.
              Local OCR — when configured on the server — always runs regardless of this setting,
              so turning it off doesn&apos;t disable identification, just the step where a crop
              would leave this machine.
            </p>
          </HelpTip>
        </label>

        {!ocrEnabled &&
          identifyStatus.data !== undefined &&
          (localOCR ? (
            <p className="text-xs text-neutral-500">
              Cloud step off: only this server&apos;s local OCR reads the ID crops — nothing
              leaves this machine. Pages it can&apos;t read confidently land in the orphan queue
              for manual identification.
            </p>
          ) : (
            <p className="text-xs text-amber-700">
              Cloud step off and this server has no local OCR either — no automatic reading will
              happen, so every page will wait in the orphan queue for fully manual
              identification.
            </p>
          ))}

        {regionsIncomplete ? (
          <p className="text-xs text-red-600">Draw the three ID regions above first.</p>
        ) : (
          upload.isError && <p className="text-xs text-red-600">{upload.error.message}</p>
        )}
        {upload.isSuccess && (
          <p className="text-xs text-green-700">
            Created batch with {upload.data.created} page{upload.data.created === 1 ? "" : "s"}
            {upload.data.skipped.length > 0 && `, skipped ${upload.data.skipped.length}`}.
          </p>
        )}
        {upload.isSuccess && upload.data.skipped.length > 0 && (
          <ul className="space-y-0.5 text-xs text-amber-700">
            {upload.data.skipped.map((s, i) => (
              <li key={i}>
                <span className="font-mono">{s.filename}</span>: {s.reason}
              </li>
            ))}
          </ul>
        )}

        <div className="flex justify-end">
          <Button
            type="submit"
            disabled={
              upload.isPending ||
              providerMissing ||
              (mode === "zip" ? zipFile === null : files.length === 0)
            }
          >
            {upload.isPending ? "Uploading…" : "Upload"}
          </Button>
        </div>
      </form>
    </Card>
  );
}
