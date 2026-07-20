// Provider settings: the AI endpoints grading methods call (managed in-app, D11).
// Keys are sealed server-side — only the last characters ever come back. Written
// for staff with no LLM background: presets + console walkthroughs over raw URLs.
// Lecturer/admin manage; TAs get a read-only table.

import { useState, type FormEvent, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../lib/api";
import { roleAtLeast, useMe } from "../lib/auth";
import type { ModelPricing, Provider, ProviderTestResult } from "../lib/types";
import { apiKindHelp, providerHelp, providersIntro } from "../lib/helpContent";
import { HelpTip } from "../components/HelpTip";
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
import { Pencil, Tag, Trash, Zap } from "../components/icons";

export function Providers() {
  const me = useMe();
  const canManage = roleAtLeast(me.data?.user.role, "lecturer");
  const [addOpen, setAddOpen] = useState(false);
  const [editing, setEditing] = useState<Provider | null>(null);
  const [deleting, setDeleting] = useState<Provider | null>(null);

  const list = useQuery({
    queryKey: ["providers"],
    queryFn: () => api.get<{ providers: Provider[] }>("/api/providers"),
  });

  const cols = canManage ? 8 : 7;

  return (
    <div className="mx-auto max-w-5xl space-y-4">
      <div className="flex items-center justify-between gap-3">
        <h1 className="text-lg font-semibold text-neutral-900">Providers</h1>
        {canManage && <Button onClick={() => setAddOpen(true)}>Add provider</Button>}
      </div>

      <Card>
        <p className="text-sm text-neutral-600">{providersIntro}</p>
      </Card>

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
              <TH>Endpoint</TH>
              <TH className="w-20">Key</TH>
              <TH className="w-52">Models</TH>
              <TH className="w-24 text-right">Rate</TH>
              <TH className="w-36">Verified</TH>
              <TH className="w-20">Enabled</TH>
              {canManage && <TH className="w-56" />}
            </tr>
          </thead>
          <tbody>
            {list.data.providers.length === 0 && (
              <tr>
                <TD colSpan={cols} className="text-center text-neutral-400">
                  No providers yet — add one so grading methods can call an AI model.
                </TD>
              </tr>
            )}
            {list.data.providers.map((p) => (
              <ProviderRow
                key={p.id}
                provider={p}
                canManage={canManage}
                cols={cols}
                onEdit={() => setEditing(p)}
                onDelete={() => setDeleting(p)}
              />
            ))}
          </tbody>
        </Table>
      )}

      {addOpen && <AddProviderDialog onClose={() => setAddOpen(false)} />}
      {editing && <EditProviderDialog provider={editing} onClose={() => setEditing(null)} />}
      {deleting && <DeleteProviderDialog provider={deleting} onClose={() => setDeleting(null)} />}
    </div>
  );
}

// --- rows ---------------------------------------------------------------------------

/** Date-only variant of fmtDate — badge-sized. */
function fmtDay(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

function ProviderRow({
  provider,
  canManage,
  cols,
  onEdit,
  onDelete,
}: {
  provider: Provider;
  canManage: boolean;
  cols: number;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const queryClient = useQueryClient();
  const [showPricing, setShowPricing] = useState(false);

  const test = useMutation({
    mutationFn: (model?: string) =>
      api.post<ProviderTestResult>(
        `/api/providers/${provider.id}/test`,
        model ? { model } : {},
      ),
    onSuccess: async (res) => {
      // A passing test refreshes models + the verified timestamp.
      if (res.ok) await queryClient.invalidateQueries({ queryKey: ["providers"] });
    },
  });

  const toggle = useMutation({
    mutationFn: (enabled: boolean) =>
      api.patch<Provider>(`/api/providers/${provider.id}`, { enabled }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["providers"] });
    },
  });

  const hasOutcome = test.isPending || test.isError || test.data !== undefined;

  return (
    <>
      <tr className="hover:bg-neutral-50">
        <TD className="font-medium">
          {provider.name}
          <span className="mt-0.5 block text-[10px] font-normal text-neutral-400">
            {provider.kind === "openai-compat" ? "OpenAI-style API" : "Anthropic-style API"}
          </span>
        </TD>
        <TD>
          <span className="block max-w-44 truncate font-mono text-xs text-neutral-600" title={provider.base_url}>
            {provider.base_url}
          </span>
        </TD>
        <TD className="font-mono text-xs text-neutral-600">{provider.api_key_hint}</TD>
        <TD>
          <ModelChips models={provider.models} />
        </TD>
        <TD className="text-right text-xs whitespace-nowrap tabular-nums">
          {provider.requests_per_second} rps / {provider.burst}
        </TD>
        <TD>
          {provider.last_verified_at ? (
            <Badge tone="green">verified {fmtDay(provider.last_verified_at)}</Badge>
          ) : (
            <span className="text-xs text-neutral-400">never</span>
          )}
        </TD>
        <TD>
          <input
            type="checkbox"
            aria-label={`${provider.name} enabled`}
            checked={provider.enabled}
            disabled={!canManage || toggle.isPending}
            onChange={(e) => toggle.mutate(e.target.checked)}
            className="size-3.5 accent-indigo-600 disabled:opacity-50"
          />
        </TD>
        {canManage && (
          <TD className="text-right whitespace-nowrap">
            <div className="flex items-center justify-end gap-1">
              <IconButton
                label={`Test ${provider.name}`}
                disabled={test.isPending}
                onClick={() => test.mutate(undefined)}
              >
                {test.isPending ? <Spinner className="size-3.5" /> : <Zap />}
              </IconButton>
              <IconButton
                label={`Pricing for ${provider.name}`}
                aria-expanded={showPricing}
                onClick={() => setShowPricing(!showPricing)}
              >
                <Tag />
              </IconButton>
              <IconButton label={`Edit ${provider.name}`} onClick={onEdit}>
                <Pencil />
              </IconButton>
              <IconButton variant="danger" label={`Delete ${provider.name}`} onClick={onDelete}>
                <Trash />
              </IconButton>
            </div>
          </TD>
        )}
      </tr>
      {toggle.isError && (
        <tr>
          <TD colSpan={cols} className="bg-neutral-50/60 px-4 py-2">
            <p className="text-xs text-red-600">{toggle.error.message}</p>
          </TD>
        </tr>
      )}
      {hasOutcome && (
        <tr>
          <TD colSpan={cols} className="bg-neutral-50/60 px-4 py-2">
            <TestOutcome
              pending={test.isPending}
              error={test.isError ? test.error : null}
              result={test.data ?? null}
              onRetry={(model) => test.mutate(model)}
            />
          </TD>
        </tr>
      )}
      {showPricing && (
        <tr>
          <TD colSpan={cols} className="bg-neutral-50/60 px-4 py-3">
            <PricingPanel provider={provider} />
          </TD>
        </tr>
      )}
    </>
  );
}

// --- model pricing ($/Mtok, decimal strings — trust spec §2) ---------------------------

const priceOk = (v: string) => /^\d+(\.\d{1,4})?$/.test(v.trim());

function PricingPanel({ provider }: { provider: Provider }) {
  const pricing = useQuery({
    queryKey: ["provider-pricing", provider.id],
    queryFn: () => api.get<{ pricing: ModelPricing[] }>(`/api/providers/${provider.id}/pricing`),
  });

  if (pricing.isPending) {
    return (
      <span className="inline-flex items-center gap-2 text-xs text-neutral-500">
        <Spinner className="size-3.5" /> Loading pricing…
      </span>
    );
  }
  if (pricing.isError) {
    return <p className="text-xs text-red-600">{pricing.error.message}</p>;
  }

  const byModel = new Map(pricing.data.pricing.map((row) => [row.model, row]));
  // Provider's model list first, then any priced models no longer in that list.
  const models = [
    ...provider.models,
    ...pricing.data.pricing.map((row) => row.model).filter((m) => !provider.models.includes(m)),
  ];

  return (
    <div className="space-y-2">
      <p className="text-xs text-neutral-500">
        Prices power run cost estimates and the monthly budget cap.
      </p>
      {models.length === 0 ? (
        <p className="text-xs text-neutral-400">
          No models yet — add model names in Edit (or run Test to fetch the catalog), then set
          prices here.
        </p>
      ) : (
        models.map((m) => (
          <PricingRow key={m} providerId={provider.id} model={m} current={byModel.get(m)} />
        ))
      )}
    </div>
  );
}

function PricingRow({
  providerId,
  model,
  current,
}: {
  providerId: number;
  model: string;
  current?: ModelPricing;
}) {
  const queryClient = useQueryClient();
  const [input, setInput] = useState(current?.input_usd_per_mtok ?? "");
  const [output, setOutput] = useState(current?.output_usd_per_mtok ?? "");

  const save = useMutation({
    // Decimal strings end-to-end — never Number() these.
    mutationFn: () =>
      api.put<ModelPricing>(`/api/providers/${providerId}/pricing`, {
        model,
        input_usd_per_mtok: input.trim(),
        output_usd_per_mtok: output.trim(),
      }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["provider-pricing", providerId] });
    },
  });

  const valid = priceOk(input) && priceOk(output);

  return (
    <div className="space-y-1">
      <div className="flex flex-wrap items-center gap-2">
        <span className="w-56 truncate font-mono text-[11px] text-neutral-700" title={model}>
          {model}
        </span>
        <label className="flex items-center gap-1.5 text-[11px] text-neutral-500">
          $ / M input tokens
          <Input
            inputMode="decimal"
            placeholder="e.g. 0.20"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            className="w-24 py-1 text-xs"
          />
        </label>
        <label className="flex items-center gap-1.5 text-[11px] text-neutral-500">
          $ / M output tokens
          <Input
            inputMode="decimal"
            placeholder="e.g. 1.60"
            value={output}
            onChange={(e) => setOutput(e.target.value)}
            className="w-24 py-1 text-xs"
          />
        </label>
        <Button
          variant="secondary"
          className="px-2.5 py-1 text-xs"
          disabled={!valid || save.isPending}
          title={valid ? undefined : "enter both prices as decimals, e.g. 0.20"}
          onClick={() => save.mutate()}
        >
          {save.isPending ? "Saving…" : "Save"}
        </Button>
        {!current && <Badge tone="amber">no pricing — run estimates will read unknown</Badge>}
      </div>
      {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
    </div>
  );
}

function ModelChips({ models }: { models: string[] }) {
  if (models.length === 0) return <span className="text-neutral-300">—</span>;
  return (
    // Capped width so the cell wraps chips instead of shoving into the Rate column.
    <span className="flex max-w-52 flex-wrap items-center gap-1">
      {models.slice(0, 3).map((m) => (
        <span
          key={m}
          title={m}
          className="max-w-44 truncate rounded bg-neutral-100 px-1.5 py-0.5 font-mono text-[11px] text-neutral-700"
        >
          {m}
        </span>
      ))}
      {models.length > 3 && (
        <span className="text-[11px] text-neutral-400" title={models.slice(3).join(", ")}>
          +{models.length - 3}
        </span>
      )}
    </span>
  );
}

// --- test outcome (shared by row + add dialog) ----------------------------------------

function TestOutcome({
  pending,
  error,
  result,
  onRetry,
}: {
  pending: boolean;
  error: Error | null;
  result: ProviderTestResult | null;
  onRetry: (model: string) => void;
}) {
  const [model, setModel] = useState("");

  let body: ReactNode = null;
  if (pending) {
    body = (
      <span className="inline-flex items-center gap-2 text-xs text-neutral-500">
        <Spinner className="size-3.5" /> Testing…
      </span>
    );
  } else if (error) {
    body = <p className="text-xs text-red-600">{error.message}</p>;
  } else if (result?.ok) {
    const n = result.models?.length ?? 0;
    body = (
      <p className="text-xs font-medium text-green-700">
        {n > 0
          ? `Works — ${n} model${n === 1 ? "" : "s"} available.`
          : result.tested_model
            ? `Works — tested ${result.tested_model}.`
            : "Works."}
      </p>
    );
  } else if (result) {
    body = <p className="text-xs text-red-600">{result.error ?? "Test failed."}</p>;
  }

  const needsModel =
    !pending && result !== null && !result.ok && (result.error ?? "").includes("enter a model");

  return (
    <div className="space-y-1.5">
      {body}
      {needsModel && (
        <div className="flex items-center gap-2">
          <Input
            placeholder="model id, e.g. qwen3-vl-plus"
            value={model}
            onChange={(e) => setModel(e.target.value)}
            className="w-56 py-1 text-xs"
          />
          <Button
            variant="secondary"
            className="px-2.5 py-1 text-xs"
            disabled={model.trim() === ""}
            onClick={() => onRetry(model.trim())}
          >
            Retry test
          </Button>
        </div>
      )}
    </div>
  );
}

// --- shared form bits -------------------------------------------------------------------

const csvToList = (csv: string): string[] =>
  csv
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);

/** Password-style key input with a show/hide toggle. */
function KeyInput({
  value,
  onChange,
  placeholder,
  required,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  required?: boolean;
}) {
  const [show, setShow] = useState(false);
  return (
    <span className="relative block">
      <Input
        type={show ? "text" : "password"}
        autoComplete="off"
        required={required}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        className="pr-12"
      />
      <button
        type="button"
        onClick={() => setShow(!show)}
        className="absolute top-1/2 right-2 -translate-y-1/2 text-xs font-medium text-neutral-500 hover:text-neutral-800"
      >
        {show ? "hide" : "show"}
      </button>
    </span>
  );
}

const urlOk = (v: string) => /^https?:\/\/.+/.test(v.trim());
const rateOk = (rps: string, burst: string) =>
  Number.isFinite(Number(rps)) &&
  Number(rps) > 0 &&
  /^\d+$/.test(burst.trim()) &&
  Number(burst) >= 1;

// --- add dialog (preset picker → form → auto-test) ---------------------------------------

type ProviderKind = "anthropic-compat" | "openai-compat";

interface Preset {
  key: "qwen" | "openrouter" | "deepseek" | "anthropic" | "custom";
  label: string;
  blurb: string;
  name: string;
  kind: ProviderKind;
  baseUrl: string;
  models: string[];
  guide: ReactNode;
}

const PRESETS: Preset[] = [
  {
    key: "qwen",
    label: "Qwen (Alibaba Model Studio)",
    blurb: "Vision models — recommended for handwritten answers",
    name: "qwen",
    kind: "anthropic-compat",
    baseUrl: "https://dashscope-intl.aliyuncs.com/apps/anthropic",
    models: ["qwen3-vl-plus"],
    guide: (
      <>
        <ol className="list-decimal space-y-0.5 pl-4">
          <li>Sign in at modelstudio.console.alibabacloud.com</li>
          <li>API Keys → Create</li>
          <li>Paste the key (starts with sk-) below</li>
        </ol>
        <p className="mt-1">Vision-capable model: qwen3-vl-plus.</p>
      </>
    ),
  },
  {
    key: "openrouter",
    label: "OpenRouter",
    blurb: "One key, models from many companies",
    name: "openrouter",
    kind: "openai-compat",
    baseUrl: "https://openrouter.ai/api/v1",
    // Curated from OpenRouter's live catalog (2026-07): cheap reasoning-capable
    // vision models first, one premium arbiter last. See docs/MODELS.md.
    models: [
      "qwen/qwen3.5-flash-02-23",
      "openai/gpt-5-nano",
      "google/gemini-3.1-flash-lite",
      "google/gemma-4-26b-a4b-it:free",
      "anthropic/claude-sonnet-5",
    ],
    guide: (
      <>
        <ol className="list-decimal space-y-0.5 pl-4">
          <li>Sign in at openrouter.ai</li>
          <li>Keys → Create Key</li>
          <li>Paste the key (starts with sk-or-) below</li>
        </ol>
        <p className="mt-1">
          One key gives access to models from many companies (Google, Anthropic, OpenAI, Qwen…).
          Model names carry the company prefix. The suggestions below are vision models that can
          <em> reason</em>, from very cheap (fractions of a cent per answer — the{" "}
          <code className="font-mono text-xs">:free</code> one costs nothing but is rate-limited)
          up to a premium pick for arbitration. &quot;Test&quot; fetches the live catalog; see
          docs/MODELS.md for prices.
        </p>
      </>
    ),
  },
  {
    key: "deepseek",
    label: "DeepSeek",
    blurb: "Text-only models — fine for experiments",
    name: "deepseek",
    kind: "anthropic-compat",
    baseUrl: "https://api.deepseek.com/anthropic",
    models: [],
    guide: (
      <>
        <p>Keys at platform.deepseek.com/api_keys.</p>
        <p className="mt-1">
          Note: DeepSeek&apos;s current models are text-only — fine for experiments, but grading
          handwriting needs a vision model (e.g. Qwen&apos;s qwen3-vl-plus).
        </p>
      </>
    ),
  },
  {
    key: "anthropic",
    label: "Anthropic (Claude)",
    blurb: "Claude models",
    name: "anthropic",
    kind: "anthropic-compat",
    baseUrl: "https://api.anthropic.com",
    models: ["claude-sonnet-5"],
    guide: <p>Keys at console.anthropic.com → API Keys.</p>,
  },
  {
    key: "custom",
    label: "Custom",
    blurb: "Any Anthropic- or OpenAI-compatible endpoint",
    name: "",
    kind: "openai-compat",
    baseUrl: "",
    models: [],
    guide: (
      <p>
        Any endpoint speaking the Anthropic Messages API or the OpenAI Chat Completions API works
        (pick which below). You need its base URL and an API key.
      </p>
    ),
  },
];

function AddProviderDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const [step, setStep] = useState<"preset" | "form" | "test">("preset");
  const [presetKey, setPresetKey] = useState<Preset["key"]>("qwen");
  const preset = PRESETS.find((p) => p.key === presetKey) ?? PRESETS[0];

  const [name, setName] = useState("");
  const [kind, setKind] = useState<ProviderKind>("anthropic-compat");
  const [baseUrl, setBaseUrl] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [modelsCsv, setModelsCsv] = useState("");
  const [rps, setRps] = useState("1");
  const [burst, setBurst] = useState("2");
  const [created, setCreated] = useState<Provider | null>(null);
  const [newKey, setNewKey] = useState(""); // failed-test key retype (D: no dead end)

  const test = useMutation({
    mutationFn: ({ id, model }: { id: number; model?: string }) =>
      api.post<ProviderTestResult>(`/api/providers/${id}/test`, model ? { model } : {}),
    onSuccess: async (res) => {
      if (res.ok) await queryClient.invalidateQueries({ queryKey: ["providers"] });
    },
  });

  // A failed key test used to dead-end at "Done" with the provider already created.
  // This PATCHes the retyped key onto that SAME provider (never a second POST — no
  // duplicate) and immediately re-tests it.
  const updateKey = useMutation({
    mutationFn: () => {
      if (!created) throw new Error("no provider to update");
      return api.patch<Provider>(`/api/providers/${created.id}`, { api_key: newKey.trim() });
    },
    onSuccess: async (p) => {
      setCreated(p);
      setNewKey("");
      await queryClient.invalidateQueries({ queryKey: ["providers"] });
      test.mutate({ id: p.id });
    },
  });

  const create = useMutation({
    mutationFn: () =>
      api.post<Provider>("/api/providers", {
        name: name.trim(),
        kind,
        base_url: baseUrl.trim(),
        api_key: apiKey,
        models: csvToList(modelsCsv),
        requests_per_second: Number(rps),
        burst: Number(burst),
      }),
    onSuccess: async (p) => {
      setCreated(p);
      setStep("test");
      await queryClient.invalidateQueries({ queryKey: ["providers"] });
      test.mutate({ id: p.id });
    },
  });

  const applyPreset = () => {
    setName(preset.name);
    setKind(preset.kind);
    setBaseUrl(preset.baseUrl);
    setModelsCsv(preset.models.join(", "));
    setStep("form");
  };

  const valid =
    /^[a-z0-9][a-z0-9-]{1,31}$/.test(name.trim()) &&
    urlOk(baseUrl) &&
    apiKey.trim() !== "" &&
    rateOk(rps, burst);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (valid) create.mutate();
  };

  // Test-step failure detection: a non-ok result (HTTP 200 with ok:false) or a
  // thrown error. The "enter a model" case has its own retry inside TestOutcome, so
  // the key-retype affordance below covers every OTHER failure (typically a bad key).
  const testMsg = test.isError
    ? test.error.message
    : test.data && !test.data.ok
      ? test.data.error ?? ""
      : null;
  const testFailed = testMsg !== null;
  const needsModel = testFailed && testMsg.includes("enter a model");

  return (
    <Dialog open onClose={onClose} title="Add provider" className="max-w-lg">
      {step === "preset" && (
        <div className="space-y-3">
          <p className="text-sm text-neutral-600">Where will grading requests go?</p>
          <div className="grid grid-cols-2 gap-2">
            {PRESETS.map((p) => (
              <button
                key={p.key}
                type="button"
                onClick={() => setPresetKey(p.key)}
                className={cx(
                  "rounded-md border p-3 text-left transition-colors",
                  presetKey === p.key
                    ? "border-indigo-600 bg-indigo-50/60 ring-1 ring-indigo-600"
                    : "border-neutral-200 hover:border-neutral-300 hover:bg-neutral-50",
                )}
              >
                <span className="block text-sm font-medium text-neutral-900">{p.label}</span>
                <span className="mt-0.5 block text-xs text-neutral-500">{p.blurb}</span>
              </button>
            ))}
          </div>
          <div className="flex justify-end gap-2 pt-1">
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button onClick={applyPreset}>Continue</Button>
          </div>
        </div>
      )}

      {step === "form" && (
        <form className="space-y-3" onSubmit={submit}>
          <div className="rounded-md bg-indigo-50/60 px-3 py-2 text-xs text-neutral-700 ring-1 ring-indigo-100 ring-inset">
            {preset.guide}
          </div>
          <div className="grid grid-cols-2 gap-3">
            <Field
              label={
                <>
                  Name <HelpTip title="Provider name">{providerHelp.name}</HelpTip>
                </>
              }
            >
              <Input
                required
                autoFocus={presetKey === "custom"}
                placeholder="e.g. qwen"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
            <Field
              label={
                <>
                  Base URL <HelpTip title="Base URL">{providerHelp.baseUrl}</HelpTip>
                </>
              }
            >
              <Input
                required
                placeholder="https://…"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
              />
            </Field>
          </div>
          {presetKey === "custom" ? (
            <Field
              label={
                <>
                  API style <HelpTip title="API style (kind)">{apiKindHelp}</HelpTip>
                </>
              }
            >
              <Select value={kind} onChange={(e) => setKind(e.target.value as ProviderKind)}>
                <option value="openai-compat">OpenAI-compatible (Chat Completions)</option>
                <option value="anthropic-compat">Anthropic-compatible (Messages)</option>
              </Select>
            </Field>
          ) : (
            <p className="text-xs text-neutral-400">
              API style: {kind === "openai-compat" ? "OpenAI-compatible" : "Anthropic-compatible"}{" "}
              (set by the preset)
            </p>
          )}
          <Field
            label={
              <>
                API key <HelpTip title="API key">{providerHelp.apiKey}</HelpTip>
              </>
            }
          >
            <KeyInput
              required
              value={apiKey}
              onChange={setApiKey}
              placeholder="paste the key from the provider console"
            />
          </Field>
          <Field
            label={
              <>
                Models <HelpTip title="Models">{providerHelp.models}</HelpTip>
              </>
            }
          >
            <Input
              placeholder="comma-separated, e.g. qwen3-vl-plus"
              value={modelsCsv}
              onChange={(e) => setModelsCsv(e.target.value)}
            />
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field
              label={
                <>
                  Requests per second{" "}
                  <HelpTip title="Rate limit">{providerHelp.rateLimit}</HelpTip>
                </>
              }
            >
              <Input
                required
                inputMode="decimal"
                value={rps}
                onChange={(e) => setRps(e.target.value)}
              />
            </Field>
            <Field
              label={
                <>
                  Burst <HelpTip title="Rate limit">{providerHelp.rateLimit}</HelpTip>
                </>
              }
            >
              <Input
                required
                inputMode="numeric"
                value={burst}
                onChange={(e) => setBurst(e.target.value)}
              />
            </Field>
          </div>
          {create.isError && <p className="text-xs text-red-600">{create.error.message}</p>}
          <div className="flex justify-end gap-2 pt-1">
            <Button variant="secondary" onClick={() => setStep("preset")}>
              Back
            </Button>
            <Button type="submit" disabled={create.isPending || !valid}>
              {create.isPending ? "Creating…" : "Create & test"}
            </Button>
          </div>
        </form>
      )}

      {step === "test" && created && (
        <div className="space-y-3">
          <p className="text-sm text-neutral-600">
            Provider <span className="font-medium text-neutral-900">{created.name}</span> saved —
            checking the endpoint and key work.
          </p>
          <TestOutcome
            pending={test.isPending}
            error={test.isError ? test.error : null}
            result={test.data ?? null}
            onRetry={(model) => test.mutate({ id: created.id, model })}
          />
          {testFailed && !needsModel && (
            <div className="space-y-2 rounded-md border border-neutral-200 bg-neutral-50 px-3 py-2.5">
              <p className="text-xs text-neutral-600">
                If the key was mistyped, retype it and test again. This updates the saved
                provider — it won&apos;t create a duplicate.
              </p>
              <KeyInput
                value={newKey}
                onChange={setNewKey}
                placeholder={`new key (currently ${created.api_key_hint})`}
              />
              <div className="flex items-center gap-2">
                <Button
                  className="px-2.5 py-1 text-xs"
                  disabled={updateKey.isPending || test.isPending || newKey.trim() === ""}
                  onClick={() => updateKey.mutate()}
                >
                  {updateKey.isPending ? "Saving…" : "Save key & retest"}
                </Button>
                <Button
                  variant="secondary"
                  className="px-2.5 py-1 text-xs"
                  disabled={test.isPending || updateKey.isPending}
                  onClick={() => test.mutate({ id: created.id })}
                >
                  Retest
                </Button>
              </div>
              {updateKey.isError && (
                <p className="text-xs text-red-600">{updateKey.error.message}</p>
              )}
            </div>
          )}
          <div className="flex justify-end pt-1">
            <Button onClick={onClose}>Done</Button>
          </div>
        </div>
      )}
    </Dialog>
  );
}

// --- edit dialog --------------------------------------------------------------------------

function EditProviderDialog({ provider, onClose }: { provider: Provider; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [baseUrl, setBaseUrl] = useState(provider.base_url);
  const [apiKey, setApiKey] = useState("");
  const [modelsCsv, setModelsCsv] = useState(provider.models.join(", "));
  const [rps, setRps] = useState(String(provider.requests_per_second));
  const [burst, setBurst] = useState(String(provider.burst));

  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = {
        base_url: baseUrl.trim(),
        models: csvToList(modelsCsv),
        requests_per_second: Number(rps),
        burst: Number(burst),
      };
      if (apiKey.trim() !== "") body.api_key = apiKey; // empty = keep current key
      return api.patch<Provider>(`/api/providers/${provider.id}`, body);
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["providers"] });
      onClose();
    },
  });

  const valid = urlOk(baseUrl) && rateOk(rps, burst);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (valid) save.mutate();
  };

  return (
    <Dialog open onClose={onClose} title={`Edit ${provider.name}`} className="max-w-lg">
      <form className="space-y-3" onSubmit={submit}>
        <Field
          label={
            <>
              Base URL <HelpTip title="Base URL">{providerHelp.baseUrl}</HelpTip>
            </>
          }
        >
          <Input required value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} />
        </Field>
        <Field
          label={
            <>
              API key <HelpTip title="API key">{providerHelp.apiKey}</HelpTip>
            </>
          }
        >
          <KeyInput
            value={apiKey}
            onChange={setApiKey}
            placeholder={`leave blank to keep the current key (${provider.api_key_hint})`}
          />
        </Field>
        <Field
          label={
            <>
              Models <HelpTip title="Models">{providerHelp.models}</HelpTip>
            </>
          }
        >
          <Input
            placeholder="comma-separated"
            value={modelsCsv}
            onChange={(e) => setModelsCsv(e.target.value)}
          />
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field
            label={
              <>
                Requests per second <HelpTip title="Rate limit">{providerHelp.rateLimit}</HelpTip>
              </>
            }
          >
            <Input
              required
              inputMode="decimal"
              value={rps}
              onChange={(e) => setRps(e.target.value)}
            />
          </Field>
          <Field
            label={
              <>
                Burst <HelpTip title="Rate limit">{providerHelp.rateLimit}</HelpTip>
              </>
            }
          >
            <Input
              required
              inputMode="numeric"
              value={burst}
              onChange={(e) => setBurst(e.target.value)}
            />
          </Field>
        </div>
        {save.isError && <p className="text-xs text-red-600">{save.error.message}</p>}
        <div className="flex justify-end gap-2 pt-1">
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={save.isPending || !valid}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// --- delete dialog --------------------------------------------------------------------------

function DeleteProviderDialog({
  provider,
  onClose,
}: {
  provider: Provider;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const del = useMutation({
    mutationFn: () => api.del(`/api/providers/${provider.id}`, {}),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["providers"] });
      onClose();
    },
  });

  return (
    <Dialog open onClose={onClose} title={`Delete ${provider.name}`}>
      <p className="text-sm text-neutral-600">
        This removes the provider and its encrypted key. If any grading method references it, the
        server refuses — disable it instead so history stays intact.
      </p>
      {del.isError && <p className="mt-2 text-xs text-red-600">{del.error.message}</p>}
      <div className="mt-4 flex justify-end gap-2">
        <Button variant="secondary" onClick={onClose}>
          Cancel
        </Button>
        <Button variant="danger" disabled={del.isPending} onClick={() => del.mutate()}>
          {del.isPending ? "Deleting…" : "Delete"}
        </Button>
      </div>
    </Dialog>
  );
}
