// Data-only CPA catalog loader. Transport URLs and credentials stay local.
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const LIMIT = 1024 * 1024;
const levels = new Set(['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max']);
const apis = new Set(['openai-responses', 'openai-completions', 'anthropic-messages']);
const object = x => x !== null && typeof x === 'object' && !Array.isArray(x);
const require = (ok, msg) => { if (!ok) throw new Error(msg); };
function keys(obj, allowed) {
  require(object(obj) && Object.keys(obj).every(k => allowed.includes(k)), 'Unsupported catalog field');
}
const safeText = (x, max = 250) => typeof x === 'string' && x.length > 0 && x.length <= max && !/[\x00-\x1f\x7f]/.test(x);
const positive = x => Number.isSafeInteger(x) && x > 0 && x <= 10_000_000;

export function validateCatalog(doc, profile) {
  keys(doc, ['schema_version', 'revision', 'profile', 'models']);
  require(doc.schema_version === 1 && /^[a-f0-9]{64}$/.test(doc.revision) && doc.profile === profile, 'Invalid catalog envelope');
  require(Array.isArray(doc.models) && doc.models.length > 0 && doc.models.length <= 500, 'Invalid model count');
  const ids = new Set();
  for (const m of doc.models) {
    keys(m, ['id', 'name', 'context_window', 'max_output_tokens', 'input_modalities', 'reasoning', 'cost', 'pi', 'provenance']);
    require(safeText(m.id) && safeText(m.name) && !ids.has(m.id), 'Invalid or duplicate model ID');
    ids.add(m.id);
    require(positive(m.context_window) && positive(m.max_output_tokens) && m.max_output_tokens <= m.context_window, 'Invalid token limits');
    require(Array.isArray(m.input_modalities) && m.input_modalities.includes('text') && m.input_modalities.every(x => ['text', 'image'].includes(x)), 'Invalid input types');
    require(typeof m.reasoning === 'boolean', 'Invalid reasoning flag');
    const costKeys = ['input', 'output', 'cacheRead', 'cacheWrite'];
    keys(m.cost, [...costKeys, 'tiers']);
    const tiers = m.cost.tiers ?? [];
    require(Array.isArray(tiers) && tiers.length <= 20, 'Invalid price tiers');
    for (const t of tiers) {
      keys(t, [...costKeys, 'inputTokensAbove']);
      require(positive(t.inputTokensAbove), 'Invalid price threshold');
    }
    for (const c of [m.cost, ...tiers]) require(costKeys.every(k => typeof c[k] === 'number' && Number.isFinite(c[k]) && c[k] >= 0 && c[k] < 1_000_000), 'Invalid cost');
    keys(m.pi, ['api', 'headers', 'compat', 'thinkingLevelMap']);
    require(apis.has(m.pi.api), 'Unsupported API');
    keys(m.pi.headers ?? {}, ['Originator', 'Version']);
    require(Object.values(m.pi.headers ?? {}).every(v => safeText(v, 199)), 'Invalid client header');
    keys(m.pi.compat ?? {}, ['sendSessionAffinityHeaders', 'supportsDeveloperRole', 'supportsOpenAIGrammarTools']);
    require(Object.values(m.pi.compat ?? {}).every(v => typeof v === 'boolean'), 'Invalid compatibility option');
    keys(m.pi.thinkingLevelMap ?? {}, [...levels]);
    require(Object.values(m.pi.thinkingLevelMap ?? {}).every(v => v === null || levels.has(v) || v === 'none'), 'Invalid thinking level');
  }
  return doc;
}

export function toPiModels(doc, baseUrl) {
  return doc.models.map(m => ({
    id: m.id, name: m.name, ...m.pi,
    baseUrl: m.pi.api === 'anthropic-messages' ? baseUrl : `${baseUrl.replace(/\/$/, '')}/v1`,
    reasoning: m.reasoning, input: m.input_modalities, cost: m.cost,
    contextWindow: m.context_window, maxTokens: m.max_output_tokens,
  }));
}

async function readJSON(file) {
  const stat = await fs.stat(file);
  require(stat.size <= LIMIT, 'Catalog file too large');
  return JSON.parse(await fs.readFile(file, 'utf8'));
}
async function atomicJSON(file, doc) {
  await fs.mkdir(path.dirname(file), { recursive: true, mode: 0o700 });
  const tmp = `${file}.${crypto.randomUUID()}.tmp`;
  try {
    await fs.writeFile(tmp, JSON.stringify(doc), { mode: 0o600 });
    await fs.rename(tmp, file);
  } finally { await fs.rm(tmp, { force: true }); }
}
async function responseJSON(response) {
  require(response.body, 'Empty response');
  const reader = response.body.getReader();
  const chunks = []; let size = 0;
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      size += value.length;
      require(size <= LIMIT, 'Catalog response too large');
      chunks.push(value);
    }
  } finally { await reader.cancel().catch(() => {}); }
  return JSON.parse(Buffer.concat(chunks).toString('utf8'));
}

export async function loadCatalog(options = {}) {
  const dir = path.dirname(fileURLToPath(import.meta.url));
  const config = options.config ?? await readJSON(path.join(os.homedir(), '.pi/agent/cpa-catalog.json'));
  keys(config, ['url', 'profile']);
  require(/^[a-z0-9_-]{1,64}$/.test(config.profile), 'Invalid profile');
  const url = new URL(config.url);
  require(!url.username && !url.password && !url.hash, 'Invalid catalog URL');
  require(url.protocol === 'https:' || (url.protocol === 'http:' && ['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname)), 'Catalog requires HTTPS or loopback');
  url.searchParams.set('profile', config.profile);
  const identity = crypto.createHash('sha256').update(url.href).digest('hex');
  const cacheFile = path.join(options.cacheDir ?? path.join(os.homedir(), '.cache/cpa-catalog'), `${identity}.json`);
  const seedFile = options.seedFile ?? path.join(dir, 'seed.json');
  let cache;
  try {
    cache = await readJSON(cacheFile);
    validateCatalog(cache.catalog, config.profile);
    require(safeText(cache.etag, 200) && typeof cache.checkedAt === 'string', 'Invalid cache');
  } catch { cache = undefined; }
  let warning;
  try {
    const apiKey = options.apiKey ?? process.env.CPA_API_KEY;
    require(apiKey, 'CPA_API_KEY is unavailable');
    const response = await (options.fetch ?? fetch)(url, {
      headers: { Authorization: `Bearer ${apiKey}`, ...(cache ? { 'If-None-Match': cache.etag } : {}) },
      redirect: 'error', signal: AbortSignal.timeout(options.timeoutMs ?? 4000),
    });
    let catalog, etag;
    if (response.status === 304 && cache) {
      catalog = cache.catalog; etag = cache.etag;
    } else {
      require(response.status === 200, `Catalog HTTP ${response.status}`);
      catalog = validateCatalog(await responseJSON(response), config.profile);
      etag = response.headers.get('etag');
      require(safeText(etag, 200), 'Missing catalog ETag');
    }
    const checkedAt = new Date().toISOString();
    try { await atomicJSON(cacheFile, { catalog, etag, checkedAt }); }
    catch { warning = 'Could not save the offline catalog cache'; }
    return { catalog, source: response.status === 304 ? 'server (unchanged)' : 'server', checkedAt, warning };
  } catch (error) {
    // Never include response bodies, headers, keys, or URLs in user-visible errors.
    warning = error instanceof Error && /^(Catalog HTTP \d+|CPA_API_KEY is unavailable)$/.test(error.message)
      ? error.message : 'Catalog request failed or returned invalid data';
  }
  if (cache) return { catalog: cache.catalog, source: 'offline cache', checkedAt: cache.checkedAt, warning };
  const catalog = validateCatalog(await readJSON(seedFile), config.profile);
  return { catalog, source: 'bundled seed', checkedAt: null, warning };
}
