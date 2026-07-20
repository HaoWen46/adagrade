// Grading methods: config-as-data (provider/model/prompt knobs, plan §4).
// List + create; row click expands the version history, where new versions are
// appended (versions are immutable — runs pin a version id, D5) and the exact
// prompt for any problem can be previewed.

import { useState, type FormEvent, type ReactNode } from "react";
import { Link } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import type {
  Assessment,
  AssessmentDetailResponse,
  GradingPolicy,
  Method,
  MethodConfig,
  MethodDetailResponse,
  MethodVersion,
  PromptPreview,
  PromptTemplate,
  Provider,
} from "../lib/types";
import { fmtDate } from "../lib/format";
import {
  gradingPolicyHelp,
  maxTokensHelp,
  methodHelp,
  methodVersionsHelp,
  reasoningLevelHelp,
} from "../lib/helpContent";
import { HelpTip } from "../components/HelpTip";
import { PolicyBadge } from "../components/PolicyBadge";
import { Archive as ArchiveIcon } from "../components/icons";
import {
  Badge,
  Button,
  Card,
  Dialog,
  Field,
  IconButton,
  Input,
  Select,
  Spinner,
  TD,
  TH,
  Table,
  cx,
} from "../components/ui";

const DEFAULT_POLICY = "standard";

const COLS = 5;

export function Methods() {
  const [includeArchived, setIncludeArchived] = useState(false);
  const [createOpen, setCreateOpen] = useState(false);
  const [expandedId, setExpandedId] = useState<number | null>(null);

  const list = useQuery({
    queryKey: ["methods", includeArchived],
    queryFn: () =>
      api.get<{ methods: Method[] }>(
        includeArchived ? "/api/methods?include_archived=1" : "/api/methods",
      ),
  });

  return (
    <div className="mx-auto max-w-4xl space-y-4">
      <div className="flex items-center justify-between gap-3">
        <h1 className="text-lg font-semibold text-neutral-900">Methods</h1>
        <div className="flex items-center gap-4">
          <label className="flex items-center gap-1.5 text-sm text-neutral-600">
            <input
              type="checkbox"
              checked={includeArchived}
              onChange={(e) => setIncludeArchived(e.target.checked)}
              className="size-3.5 accent-indigo-600"
            />
            Include archived
          </label>
          <Button onClick={() => setCreateOpen(true)}>New method</Button>
        </div>
      </div>

      {list.isPending ? (
        <div className="flex justify-center py-10">
          <Spinner className="size-6" />
        </div>
      ) : list.isError ? (
        <Card>
          <p className="text-sm text-red-600">{list.error.message}</p>
        </Card>
      ) : (
        <Table>
          <thead>
            <tr>
              <TH>Name</TH>
              <TH>Provider / model</TH>
              <TH className="w-28 text-right">Temperature</TH>
              <TH className="w-20 text-right">Version</TH>
              <TH className="w-24">Status</TH>
            </tr>
          </thead>
          <tbody>
            {list.data.methods.length === 0 && (
              <tr>
                <TD colSpan={COLS} className="text-center text-neutral-400">
                  No grading methods yet.
                </TD>
              </tr>
            )}
            {list.data.methods.map((m) => (
              <MethodRow
                key={m.id}
                method={m}
                expanded={expandedId === m.id}
                onToggle={() => setExpandedId(expandedId === m.id ? null : m.id)}
              />
            ))}
          </tbody>
        </Table>
      )}

      {createOpen && <MethodDialog onClose={() => setCreateOpen(false)} />}
    </div>
  );
}

// --- rows ---------------------------------------------------------------------------

function MethodRow({
  method,
  expanded,
  onToggle,
}: {
  method: Method;
  expanded: boolean;
  onToggle: () => void;
}) {
  const cfg = method.latest?.config;
  return (
    <>
      <tr onClick={onToggle} className="cursor-pointer hover:bg-neutral-50">
        <TD className="font-medium">
          <span className="mr-1.5 inline-block w-3 text-neutral-400">{expanded ? "▾" : "▸"}</span>
          {method.name}
        </TD>
        <TD>
          {cfg ? (
            <span className="font-mono text-xs">
              {cfg.provider} / {cfg.model}
            </span>
          ) : (
            <span className="text-neutral-300">—</span>
          )}
        </TD>
        <TD className="text-right tabular-nums">{cfg ? cfg.temperature : "—"}</TD>
        <TD className="text-right tabular-nums">
          {method.latest ? `v${method.latest.version}` : "—"}
        </TD>
        <TD>
          {method.archived ? (
            <Badge tone="amber">archived</Badge>
          ) : (
            <Badge tone="green">active</Badge>
          )}
        </TD>
      </tr>
      {expanded && (
        <tr>
          <TD colSpan={COLS} className="bg-neutral-50/60 px-4 py-3">
            <MethodDetail method={method} />
          </TD>
        </tr>
      )}
    </>
  );
}

// --- expanded detail: version history + archive + new version -----------------------

function MethodDetail({ method }: { method: Method }) {
  const queryClient = useQueryClient();
  const [versionOpen, setVersionOpen] = useState(false);

  const detail = useQuery({
    queryKey: ["method", method.id],
    queryFn: () => api.get<MethodDetailResponse>(`/api/methods/${method.id}`),
  });

  const archive = useMutation({
    mutationFn: () =>
      api.post<Method>(`/api/methods/${method.id}/archive`, { archived: !method.archived }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["methods"] });
      await queryClient.invalidateQueries({ queryKey: ["method", method.id] });
    },
  });

  if (detail.isPending) {
    return <Spinner />;
  }
  if (detail.isError) {
    return <p className="text-sm text-red-600">{detail.error.message}</p>;
  }

  const versions = detail.data.versions; // newest first
  const latest = versions[0];

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <h3 className="text-sm font-semibold text-neutral-900">
          <span className="inline-flex items-center gap-1.5">
            Version history <HelpTip title="Versions & archive">{methodVersionsHelp}</HelpTip>
          </span>
        </h3>
        <div className="flex items-center gap-2">
          <Button className="px-2.5 py-1 text-xs" onClick={() => setVersionOpen(true)}>
            New version
          </Button>
          <IconButton
            label={`${method.archived ? "Unarchive" : "Archive"} ${method.name}`}
            disabled={archive.isPending}
            onClick={() => archive.mutate()}
          >
            {archive.isPending ? <Spinner className="size-3.5" /> : <ArchiveIcon />}
          </IconButton>
        </div>
      </div>
      {archive.isError && <p className="text-xs text-red-600">{archive.error.message}</p>}

      {versions.length === 0 && <p className="text-sm text-neutral-400">No versions yet.</p>}
      {versions.map((v) => (
        <VersionItem key={v.id} version={v} isLatest={v.id === latest?.id} />
      ))}

      {latest && (
        <PromptPreviewSection
          method={method}
          latestVersion={latest.version}
          configuredPolicy={latest.config.policy ?? DEFAULT_POLICY}
        />
      )}

      {versionOpen && (
        <MethodDialog
          method={method}
          initial={latest?.config}
          onClose={() => setVersionOpen(false)}
        />
      )}
    </div>
  );
}

function VersionItem({ version, isLatest }: { version: MethodVersion; isLatest: boolean }) {
  return (
    <div className="rounded-md border border-neutral-200 bg-white p-3">
      <div className="flex items-center gap-2">
        <span className="text-sm font-medium text-neutral-900 tabular-nums">
          v{version.version}
        </span>
        {isLatest && <Badge tone="indigo">latest</Badge>}
        <span className="ml-auto text-xs text-neutral-400">{fmtDate(version.created_at)}</span>
      </div>
      <pre className="mt-2 rounded-md border border-neutral-200 bg-neutral-50 p-2 font-mono text-xs whitespace-pre-wrap text-neutral-700">
        {JSON.stringify(version.config, null, 2)}
      </pre>
    </div>
  );
}

// --- prompt preview: pick a problem, see the exact rendered prompt --------------------

const POLICY_KEYS = ["lenient", "standard", "strict"] as const;

function PromptPreviewSection({
  method,
  latestVersion,
  configuredPolicy,
}: {
  method: Method;
  latestVersion: number;
  configuredPolicy: string;
}) {
  const [assessmentId, setAssessmentId] = useState("");
  const [problemId, setProblemId] = useState("");
  const [policy, setPolicy] = useState(configuredPolicy);
  const [open, setOpen] = useState(false);

  const assessments = useQuery({
    queryKey: ["assessments", false],
    queryFn: () => api.get<{ assessments: Assessment[] }>("/api/assessments"),
  });
  const detail = useQuery({
    queryKey: ["assessment", assessmentId],
    queryFn: () => api.get<AssessmentDetailResponse>(`/api/assessments/${assessmentId}`),
    enabled: assessmentId !== "",
  });

  const preview = useMutation({
    mutationFn: () =>
      api.get<PromptPreview>(
        `/api/problems/${problemId}/prompt-preview?method_id=${method.id}&policy=${policy}`,
      ),
    onSuccess: () => setOpen(true),
  });

  return (
    <div className="rounded-md border border-neutral-200 bg-white p-3">
      <h4 className="text-sm font-semibold text-neutral-900">Preview prompt</h4>
      <p className="mt-0.5 text-xs text-neutral-500">
        Renders the exact instructions v{latestVersion} would send for a problem — what the model
        actually sees.
      </p>
      <div className="mt-2 flex flex-wrap items-end gap-2">
        <Field label="Assessment" className="w-52">
          <Select
            value={assessmentId}
            onChange={(e) => {
              setAssessmentId(e.target.value);
              setProblemId("");
              preview.reset();
            }}
            className="w-full"
          >
            <option value="">select…</option>
            {(assessments.data?.assessments ?? []).map((a) => (
              <option key={a.id} value={String(a.id)}>
                {a.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Problem" className="w-52">
          <Select
            value={problemId}
            disabled={assessmentId === ""}
            onChange={(e) => {
              setProblemId(e.target.value);
              preview.reset();
            }}
            className="w-full"
          >
            <option value="">select…</option>
            {(detail.data?.problems ?? []).map((p) => (
              <option key={p.id} value={String(p.id)}>
                {p.number}
                {p.title && ` — ${p.title}`}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Policy">
          <div className="inline-flex rounded-md border border-neutral-300 bg-white p-0.5">
            {POLICY_KEYS.map((k) => (
              <button
                key={k}
                type="button"
                onClick={() => {
                  setPolicy(k);
                  preview.reset();
                }}
                className={cx(
                  "rounded px-2 py-1 text-xs font-medium capitalize transition-colors",
                  policy === k
                    ? "bg-neutral-900 text-white"
                    : "text-neutral-600 hover:text-neutral-900",
                )}
              >
                {k}
              </button>
            ))}
          </div>
        </Field>
        <Button
          variant="secondary"
          className="px-2.5 py-1.5 text-xs"
          disabled={problemId === "" || preview.isPending}
          onClick={() => preview.mutate()}
        >
          {preview.isPending ? "Rendering…" : "Preview prompt"}
        </Button>
      </div>
      {preview.isError && <p className="mt-2 text-xs text-red-600">{preview.error.message}</p>}
      {open && preview.data && (
        <PromptPreviewDialog preview={preview.data} onClose={() => setOpen(false)} />
      )}
    </div>
  );
}

/** Pretty-print the schema when it parses; otherwise show it raw. */
function prettySchema(schema: string): string {
  try {
    return JSON.stringify(JSON.parse(schema), null, 2);
  } catch {
    return schema;
  }
}

function PromptPreviewDialog({
  preview,
  onClose,
}: {
  preview: PromptPreview;
  onClose: () => void;
}) {
  const { pins } = preview;
  const pinRows: Array<[string, ReactNode]> = [
    ["Provider / model", `${pins.provider} / ${pins.model}`],
    ["Temperature", String(pins.temperature)],
    ["Policy", <PolicyBadge key="policy" policy={pins.policy} />],
    ["Prompt template", `${pins.prompt_template} v${pins.prompt_template_version}`],
    ["Rubric", `v${pins.rubric_version}`],
    [
      "Reference solution",
      pins.reference_solution_version > 0 ? `v${pins.reference_solution_version}` : "not included",
    ],
  ];

  return (
    <Dialog open onClose={onClose} title="Prompt preview" className="max-w-3xl">
      <div className="space-y-3">
        <table className="w-full border-collapse text-left text-xs">
          <tbody>
            {pinRows.map(([label, value]) => (
              <tr key={label} className="border-b border-neutral-100 last:border-0">
                <td className="w-40 py-1 pr-2 font-medium text-neutral-500">{label}</td>
                <td className="py-1 font-mono text-neutral-800">{value}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <div>
          <h4 className="mb-1 text-xs font-semibold tracking-wide text-neutral-500 uppercase">
            System prompt
          </h4>
          <pre className="max-h-56 overflow-auto rounded-md border border-neutral-200 bg-neutral-50 p-2 font-mono text-xs whitespace-pre-wrap text-neutral-700">
            {preview.system}
          </pre>
        </div>
        <div>
          <h4 className="mb-1 text-xs font-semibold tracking-wide text-neutral-500 uppercase">
            User prompt
          </h4>
          <pre className="max-h-56 overflow-auto rounded-md border border-neutral-200 bg-neutral-50 p-2 font-mono text-xs whitespace-pre-wrap text-neutral-700">
            {preview.user}
          </pre>
        </div>
        <details>
          <summary className="cursor-pointer text-xs text-neutral-500 hover:text-neutral-800">
            Output schema (JSON)
          </summary>
          <pre className="mt-1 max-h-56 overflow-auto rounded-md border border-neutral-200 bg-neutral-50 p-2 font-mono text-xs whitespace-pre-wrap text-neutral-700">
            {prettySchema(preview.schema)}
          </pre>
        </details>
      </div>
    </Dialog>
  );
}

// --- create / new-version dialog (shared config form) --------------------------------

const REASONING_LEVELS = ["off", "low", "medium", "high"] as const;

/** Sentinel for the model dropdown's free-text escape hatch. */
const OTHER_MODEL = "__other__";

/** Selected-card ring/background per policy — same accent family as PolicyBadge. */
const POLICY_CARD_SELECTED: Record<string, string> = {
  lenient: "border-green-400 bg-green-50/60 ring-1 ring-green-300",
  standard: "border-indigo-400 bg-indigo-50/60 ring-1 ring-indigo-300",
  strict: "border-amber-400 bg-amber-50/60 ring-1 ring-amber-300",
};

/** Three selectable cards (radio behavior) — the rubric stays untouched; this only
 * picks the ambiguity-resolution stance (see gradingPolicyHelp). */
function PolicyPicker({
  policies,
  value,
  onChange,
}: {
  policies: GradingPolicy[];
  value: string;
  onChange: (key: string) => void;
}) {
  return (
    <div role="radiogroup" aria-label="Grading policy" className="grid grid-cols-3 gap-2">
      {policies.map((p) => {
        const selected = p.key === value;
        return (
          <button
            key={p.key}
            type="button"
            role="radio"
            aria-checked={selected}
            onClick={() => onChange(p.key)}
            className={cx(
              "rounded-md border px-3 py-2 text-left transition-colors",
              selected
                ? POLICY_CARD_SELECTED[p.key]
                : "border-neutral-200 bg-white hover:bg-neutral-50",
            )}
          >
            <span className="flex items-center gap-1.5">
              <span className="text-sm font-semibold text-neutral-900">{p.label}</span>
              {selected && <PolicyBadge policy={p.key} />}
            </span>
            <p className="mt-0.5 text-xs text-neutral-600">{p.tagline}</p>
            <p className="mt-1 text-[11px] text-neutral-400">{p.when_to_use}</p>
          </button>
        );
      })}
    </div>
  );
}

function MethodDialog({
  method,
  initial,
  onClose,
}: {
  /** When present the dialog appends a version; otherwise it creates a method. */
  method?: Method;
  initial?: MethodConfig;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [provider, setProvider] = useState(initial?.provider ?? "");
  const [model, setModel] = useState(initial?.model ?? "");
  const [modelOther, setModelOther] = useState(false);
  const [temperature, setTemperature] = useState(initial ? String(initial.temperature) : "0");
  const [reasoningLevel, setReasoningLevel] = useState(initial?.reasoning_level ?? "");
  // Default on to match the seeded default method (grading/seed.go RefSolutions:1).
  const [refSolutions, setRefSolutions] = useState(initial ? initial.ref_solutions === 1 : true);
  const [reaskCap, setReaskCap] = useState(initial ? String(initial.reask_cap) : "2");
  const [policy, setPolicy] = useState(initial?.policy ?? DEFAULT_POLICY);
  const [maxTokens, setMaxTokens] = useState(
    initial?.max_tokens !== undefined ? String(initial.max_tokens) : "",
  );

  const providersQuery = useQuery({
    queryKey: ["providers"],
    queryFn: () => api.get<{ providers: Provider[] }>("/api/providers"),
  });
  const templateQuery = useQuery({
    queryKey: ["prompt-template", "transcribe-then-grade"],
    queryFn: () => api.get<PromptTemplate>("/api/prompt-templates/transcribe-then-grade"),
  });
  const policiesQuery = useQuery({
    queryKey: ["grading-policies"],
    queryFn: () => api.get<{ policies: GradingPolicy[] }>("/api/grading-policies"),
  });

  const template = templateQuery.data;
  // Prefills keep their pinned template version; fresh configs pin the latest.
  const templateVersionId = initial?.prompt_template_version_id ?? template?.id;

  const save = useMutation({
    mutationFn: (): Promise<Method | MethodVersion> => {
      const config: MethodConfig = {
        provider: provider.trim(),
        model: model.trim(),
        temperature: Number(temperature),
        ref_solutions: refSolutions ? 1 : 0,
        reask_cap: Number(reaskCap),
        policy,
        // templateVersionId is defined whenever `valid` allows submitting
        prompt_template_version_id: templateVersionId ?? 0,
      };
      if (reasoningLevel) config.reasoning_level = reasoningLevel;
      if (maxTokens.trim() !== "") config.max_tokens = Number(maxTokens);
      return method
        ? api.post<MethodVersion>(`/api/methods/${method.id}/versions`, { config })
        : api.post<Method>("/api/methods", { name: name.trim(), config });
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["methods"] });
      if (method) await queryClient.invalidateQueries({ queryKey: ["method", method.id] });
      onClose();
    },
  });

  const temp = Number(temperature);
  const valid =
    (method !== undefined || name.trim() !== "") &&
    provider.trim() !== "" &&
    model.trim() !== "" &&
    temperature.trim() !== "" &&
    Number.isFinite(temp) &&
    temp >= 0 &&
    temp <= 2 &&
    /^[0-5]$/.test(reaskCap.trim()) &&
    (maxTokens.trim() === "" || /^\d+$/.test(maxTokens.trim())) &&
    templateVersionId !== undefined;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (valid) save.mutate();
  };

  const enabledNames = (providersQuery.data?.providers ?? [])
    .filter((p) => p.enabled)
    .map((p) => p.name);
  // Keep an out-of-registry provider selectable when editing an old config.
  const providerOptions =
    provider !== "" && !enabledNames.includes(provider)
      ? [provider, ...enabledNames]
      : enabledNames;

  // Models suggested by the selected provider; keep an off-list model selectable.
  const providerModels =
    providersQuery.data?.providers.find((p) => p.name === provider)?.models ?? [];
  const modelOptions =
    model !== "" && !providerModels.includes(model) ? [model, ...providerModels] : providerModels;

  return (
    <Dialog
      open
      onClose={onClose}
      title={method ? `New version of ${method.name}` : "New method"}
    >
      <form className="space-y-3" onSubmit={submit}>
        {!method && (
          <Field label="Name">
            <Input
              required
              autoFocus
              placeholder="e.g. sonnet-strict"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </Field>
        )}
        <div className="grid grid-cols-2 gap-3">
          <Field
            label={
              <>
                Provider <HelpTip title="Provider">{methodHelp.provider}</HelpTip>
              </>
            }
          >
            {providersQuery.isPending ? (
              <Spinner />
            ) : providersQuery.isError ? (
              <p className="text-xs text-red-600">{providersQuery.error.message}</p>
            ) : providerOptions.length > 0 ? (
              <Select
                required
                value={provider}
                onChange={(e) => {
                  setProvider(e.target.value);
                  setModel("");
                  setModelOther(false);
                }}
                className="w-full"
              >
                <option value="">select…</option>
                {providerOptions.map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </Select>
            ) : (
              <p className="rounded-md bg-amber-50 px-3 py-2 text-xs text-amber-800 ring-1 ring-amber-200 ring-inset">
                No enabled providers — add one under{" "}
                <Link to="/providers" className="font-medium underline">
                  Providers
                </Link>
                .
              </p>
            )}
          </Field>
          <Field
            label={
              <>
                Model <HelpTip title="Model">{methodHelp.model}</HelpTip>
              </>
            }
          >
            {modelOptions.length > 0 && !modelOther ? (
              <Select
                required
                value={model}
                onChange={(e) => {
                  if (e.target.value === OTHER_MODEL) {
                    setModelOther(true);
                    setModel("");
                  } else {
                    setModel(e.target.value);
                  }
                }}
                className="w-full"
              >
                <option value="">select…</option>
                {modelOptions.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
                <option value={OTHER_MODEL}>Other…</option>
              </Select>
            ) : (
              <span className="flex items-center gap-1.5">
                <Input
                  required
                  placeholder="model id"
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                />
                {modelOptions.length > 0 && (
                  <Button
                    variant="ghost"
                    className="px-2 py-1 text-xs"
                    onClick={() => {
                      setModelOther(false);
                      setModel("");
                    }}
                  >
                    list
                  </Button>
                )}
              </span>
            )}
          </Field>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <Field
            label={
              <>
                Temperature <HelpTip title="Temperature">{methodHelp.temperature}</HelpTip>
              </>
            }
          >
            <Input
              required
              type="number"
              step={0.1}
              min={0}
              max={2}
              value={temperature}
              onChange={(e) => setTemperature(e.target.value)}
            />
          </Field>
          <Field
            label={
              <>
                Reasoning level <HelpTip title="Reasoning level">{reasoningLevelHelp}</HelpTip>
              </>
            }
          >
            <Select
              value={reasoningLevel}
              onChange={(e) => setReasoningLevel(e.target.value)}
              className="w-full"
            >
              <option value="">default</option>
              {REASONING_LEVELS.map((l) => (
                <option key={l} value={l}>
                  {l}
                </option>
              ))}
            </Select>
          </Field>
        </div>
        <label className="flex items-center gap-1.5 text-sm text-neutral-600">
          <input
            type="checkbox"
            checked={refSolutions}
            onChange={(e) => setRefSolutions(e.target.checked)}
            className="size-3.5 accent-indigo-600"
          />
          Include reference solutions (recommended){" "}
          <HelpTip title="Reference solutions">{methodHelp.refSolutions}</HelpTip>
        </label>
        <div className="grid grid-cols-2 gap-3">
          <Field
            label={
              <>
                Re-ask cap <HelpTip title="Re-ask cap">{methodHelp.reaskCap}</HelpTip>
              </>
            }
          >
            <Input
              required
              type="number"
              min={0}
              max={5}
              value={reaskCap}
              onChange={(e) => setReaskCap(e.target.value)}
            />
          </Field>
          <Field
            label={
              <>
                Max tokens (optional) <HelpTip title="Max tokens">{maxTokensHelp}</HelpTip>
              </>
            }
          >
            <Input
              inputMode="numeric"
              placeholder="blank = 4096"
              value={maxTokens}
              onChange={(e) => setMaxTokens(e.target.value)}
            />
          </Field>
        </div>
        <Field
          label={
            <>
              Grading policy <HelpTip title="Grading policy">{gradingPolicyHelp}</HelpTip>
            </>
          }
        >
          {policiesQuery.isPending ? (
            <Spinner />
          ) : policiesQuery.isError ? (
            <p className="text-xs text-red-600">{policiesQuery.error.message}</p>
          ) : (
            <PolicyPicker
              policies={policiesQuery.data.policies}
              value={policy}
              onChange={setPolicy}
            />
          )}
        </Field>
        <Field
          label={
            <>
              Prompt template{" "}
              <HelpTip title="Prompt template">{methodHelp.promptTemplate}</HelpTip>
            </>
          }
        >
          {templateQuery.isPending ? (
            <Spinner />
          ) : templateQuery.isError ? (
            <p className="text-xs text-red-600">{templateQuery.error.message}</p>
          ) : (
            <p className="py-1.5 text-sm text-neutral-800">
              {template && templateVersionId === template.id
                ? `${template.name} v${template.version}`
                : `pinned version id ${templateVersionId}`}
            </p>
          )}
        </Field>
        {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={save.isPending || !valid}>
            {save.isPending ? "Saving…" : method ? "Add version" : "Create"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
