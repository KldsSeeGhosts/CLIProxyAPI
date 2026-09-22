import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { loadCatalog, validateCatalog, toPiModels } from './client.mjs';
const seedFile = new URL('./seed.json', import.meta.url);
const seed = JSON.parse(await fs.readFile(seedFile, 'utf8'));
const cacheDir = await fs.mkdtemp(path.join(os.tmpdir(), 'cpa-catalog-test-'));
const config = { url: 'https://catalog.example/v1/catalog/pi', profile: 'personal' };
const options = { config, seedFile, cacheDir, apiKey: 'test-key' };
const unavailable = async () => { throw new Error('secret-that-must-not-be-shown'); };
try {
  let result = await loadCatalog({ ...options, fetch: unavailable });
  assert.equal(result.source, 'bundled seed');
  assert.ok(!result.warning.includes('secret'));
  result = await loadCatalog({ ...options, fetch: async (url, init) => {
    assert.equal(init.headers.Authorization, 'Bearer test-key');
    assert.equal(init.redirect, 'error');
    assert.equal(url.searchParams.get('profile'), 'personal');
    return new Response(JSON.stringify(seed), { headers: { etag: '"test"' } });
  }});
  assert.equal(result.source, 'server');
  result = await loadCatalog({ ...options, fetch: async (url, init) => {
    assert.equal(init.headers['If-None-Match'], '"test"');
    return new Response(null, { status: 304 });
  }});
  assert.equal(result.source, 'server (unchanged)');
  result = await loadCatalog({ ...options, fetch: unavailable });
  assert.equal(result.source, 'offline cache');
  const bad = structuredClone(seed); bad.models[0].pi.baseUrl = 'https://evil.invalid';
  assert.throws(() => validateCatalog(bad, 'personal'));
  result = await loadCatalog({ ...options, fetch: async () => new Response(JSON.stringify(bad), { headers: { etag: '"bad"' } }) });
  assert.equal(result.source, 'offline cache');
  const pi = toPiModels(result.catalog, 'http://127.0.0.1:8317');
  const swe = pi.find(x => x.id === 'devin/swe-2');
  assert.equal(swe.contextWindow, 262000); assert.equal(swe.maxTokens, 128000);
  assert.ok(swe.input.includes('image'));
  assert.equal(swe.thinkingLevelMap.max, 'max');
  assert.equal(pi.find(x => x.id === 'opencode-go/deepseek-v4.1-flash').maxTokens, 393216);
  assert.ok(pi.find(x => x.id === 'gpt-6-astra').cost.tiers.length);
  for (const f of await fs.readdir(cacheDir)) await fs.writeFile(path.join(cacheDir, f), '{broken');
  result = await loadCatalog({ ...options, fetch: unavailable });
  assert.equal(result.source, 'bundled seed');
  await assert.rejects(loadCatalog({ ...options, config: { ...config, url: 'http://external.invalid' }, fetch: unavailable }));
  // A server that stalls in the body must obey the same startup timeout.
  const server = http.createServer((req, res) => { res.writeHead(200, { etag: '"x"' }); res.write('{'); });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    const start = Date.now();
    result = await loadCatalog({ ...options, config: { ...config, url: `http://127.0.0.1:${server.address().port}` }, timeoutMs: 100 });
    assert.equal(result.source, 'bundled seed'); assert.ok(Date.now() - start < 2000);
  } finally { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
  console.log('Client checks passed: live, 304, offline, invalid data, corrupt cache, timeout, metadata mapping.');
} finally { await fs.rm(cacheDir, { recursive: true, force: true }); }
