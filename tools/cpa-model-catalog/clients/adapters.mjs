export function efforts(model) {
  if (!model.reasoning) return [];
  const map = model.pi.thinkingLevelMap ?? {};
  return [...new Set(['low','medium','high','xhigh','max'].flatMap(level => {
    const wire = map[level] === undefined ? (['low','medium','high'].includes(level) ? level : null) : map[level];
    return wire && wire !== 'none' ? [wire] : [];
  }))];
}
export function codexCatalog(catalog, templates, localModels = []) {
  const byId = new Map(templates.models.map(m => [m.slug, m]));
  const fallback = byId.get('gpt-5.6-sol') ?? templates.models[0];
  if (!fallback) throw new Error('A local Codex transport template is required');
  const models = catalog.models.map((m, i) => {
    const old = byId.get(m.id) ?? fallback;
    const levels = efforts(m);
    return { ...structuredClone(old), slug: m.id, display_name: m.name,
      description: `${m.name}. Managed by CPA catalog.`, priority: i + 1,
      visibility: 'list', supported_in_api: true,
      context_window: m.context_window, max_context_window: m.context_window,
      input_modalities: m.input_modalities,
      default_reasoning_level: levels.includes(old.default_reasoning_level) ? old.default_reasoning_level : (levels.includes('medium') ? 'medium' : levels[0] ?? null),
      supported_reasoning_levels: levels.map(effort => ({ effort, description: `${effort} reasoning` })),
    };
  });
  for (const m of templates.models) if (localModels.includes(m.slug) && !models.some(x => x.slug === m.slug)) models.push(structuredClone(m));
  return { models };
}
export function claudeSettings(catalog, existing = {}, replace = true) {
  const next = structuredClone(existing);
  next.modelPicker = { ...next.modelPicker, replaceBuiltInOptions: replace,
    options: catalog.models.map(m => ({ model: m.id, label: m.name,
      description: `${m.context_window} context; ${m.max_output_tokens} output; ${m.input_modalities.join('/')}; thinking: ${efforts(m).join('/') || 'provider default'}`,
      ...(m.id.startsWith('claude-opus-4-6') ? { behavesAs:'claude-opus-4-6' } : {}),
      ...(m.id.startsWith('claude-sonnet-4-6') ? { behavesAs:'claude-sonnet-4-6' } : {}),
    })) };
  return next;
}
export function openCodeConfig(catalog, baseUrl, defaultModel) {
  const provider = {};
  for (const group of ['cpa-chat','cpa-codex','cpa-claude']) {
    provider[group] = { name: `CPA ${group.slice(4)}`, npm: '@ai-sdk/openai-compatible',
      options: { baseURL: `${baseUrl.replace(/\/$/,'')}/v1`, apiKey:'{env:CPA_API_KEY}' }, models: {} };
  }
  for (const m of catalog.models) {
    const group = m.pi.api === 'anthropic-messages' ? 'cpa-claude' : m.pi.api === 'openai-responses' ? 'cpa-codex' : 'cpa-chat';
    const variants = Object.fromEntries(['none','minimal','low','medium','high','xhigh','max'].map(k => [k,{disabled:true}]));
    for (const level of efforts(m)) variants[level] = { reasoningEffort:level };
    provider[group].models[m.id] = { id:m.id, name:m.name, reasoning:m.reasoning, attachment:m.input_modalities.includes('image'), tool_call:true,
      limit:{context:m.context_window, output:m.max_output_tokens}, modalities:{input:m.input_modalities, output:['text']},
      cost:{input:m.cost.input,output:m.cost.output,cache_read:m.cost.cacheRead,cache_write:m.cost.cacheWrite},
      headers:m.pi.headers ?? {}, variants };
  }
  const all = Object.entries(provider).flatMap(([group,p]) => Object.keys(p.models).map(id => `${group}/${id}`));
  return { $schema:'https://opencode.ai/config.json', model:all.includes(defaultModel) ? defaultModel : all[0], provider };
}

export function openCodeV2Config(catalog, existing, baseUrl) {
  const next = structuredClone(existing);
  next.providers ??= {};
  const groups = ['cpa-chat','cpa-codex','cpa-claude'];
  const previousGroup = new Map(groups.flatMap(group => Object.keys(next.providers[group]?.models ?? {}).map(id => [id,group])));
  for (const group of groups) {
    next.providers[group] = { name:`CPA ${group.slice(4)}`, package:'@opencode/ai/providers/openai-compatible',
      settings:{baseURL:`${baseUrl.replace(/\/$/,'')}/v1`,apiKey:'{env:CPA_API_KEY}'},
      ...next.providers[group], env:[...new Set([...(next.providers[group]?.env??[]),'CPA_API_KEY'])], models:{} };
  }
  const price = c => ({input:c.input,output:c.output,cache:{read:c.cacheRead,write:c.cacheWrite}});
  for (const m of catalog.models) {
    const group=previousGroup.get(m.id) ?? (m.pi.api==='anthropic-messages'?'cpa-claude':m.pi.api==='openai-responses'?'cpa-codex':'cpa-chat');
    next.providers[group].models[m.id]={modelID:m.id,name:m.name,
      capabilities:{tools:true,input:m.input_modalities,output:['text']},
      limit:{context:m.context_window,output:m.max_output_tokens},
      cost:m.cost.tiers?.length ? [price(m.cost),...m.cost.tiers.map(t=>({...price(t),tier:{type:'context',size:t.inputTokensAbove}}))] : price(m.cost),
      variants:efforts(m).map(id=>({id,body:{reasoning_effort:id}}))};
  }
  return next;
}
