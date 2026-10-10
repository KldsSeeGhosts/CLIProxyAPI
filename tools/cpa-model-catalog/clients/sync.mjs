#!/usr/bin/env node
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { spawn, spawnSync } from 'node:child_process';
import { loadCatalog } from './client.mjs';
import { codexCatalog, claudeSettings, openCodeConfig, openCodeV2Config, efforts } from './adapters.mjs';
const home = os.homedir();
const configPath = process.env.CPA_SYNC_CONFIG ?? path.join(home,'.config/cpa-catalog/sync.json');
const root = path.dirname(fileURLToPath(import.meta.url));
const read = async file => JSON.parse(await fs.readFile(file,'utf8'));
const sha = text => crypto.createHash('sha256').update(text).digest('hex');
async function key() {
  if (process.env.CPA_API_KEY) return process.env.CPA_API_KEY;
  try {
    const s = await fs.readFile(path.join(home,'.config/environment.d/cpa.conf'),'utf8');
    return /^CPA_API_KEY=(.+)$/m.exec(s)?.[1]?.trim().replace(/^(['"])(.*)\1$/, '$2');
  } catch { return undefined; }
}
async function write(file, value, expected, archive = true) {
  const data=JSON.stringify(value,null,2)+'\n';
  let old; try {old=await fs.readFile(file,'utf8');} catch(e){if(e.code!=='ENOENT')throw e;}
  if (old===data) return false;
  if (expected!==undefined && old!==expected) throw new Error('Configuration changed during synchronization; retry');
  await fs.mkdir(path.dirname(file),{recursive:true,mode:0o700});
  // Preserve the exact previous file outside any harness discovery path.
  if(old!==undefined && archive) {
    const backup=path.join(home,'.local/state/cpa-catalog/backups',sha(file),`${sha(old)}.json`);
    await fs.mkdir(path.dirname(backup),{recursive:true,mode:0o700});
    await fs.writeFile(backup,old,{mode:0o600});
  }
  const temp=`${file}.${crypto.randomUUID()}.tmp`;
  try {
    await fs.writeFile(temp,data,{mode:0o600});
    // Compare again immediately before replacement, preserving concurrent user edits.
    const now=await fs.readFile(file,'utf8').catch(e=>{if(e.code==='ENOENT')return undefined;throw e;});
    if(now!==old)throw new Error('Configuration changed during synchronization; retry');
    await fs.rename(temp,file);
  } finally {await fs.rm(temp,{force:true});}
  return true;
}
async function synchronize(config, {offline=false,dryRun=false}={}) {
  const apiKey=await key();
  const state=await loadCatalog({config:{url:config.url,profile:config.profile},apiKey,
    cacheDir:config.cacheDir,seedFile:config.seedFile ?? path.join(root,'seed.json'),
    ...(offline?{fetch:async()=>{throw new Error('Offline requested');}}:{})});
  // Fallback may support startup, but must never overwrite a newer installed selection.
  if(!state.source.startsWith('server') && !offline) {
    console.error(`CPA sync retained installed settings: ${state.warning}.`);
    return {state,changed:[],retained:true};
  }
  if(offline)return {state,changed:[],retained:true};
  const outputs=[];
  if(config.codex) {
    const templates=await read(config.codex.templates);
    outputs.push([config.codex.path,codexCatalog(state.catalog,templates,config.codex.localModels??[])]);
  }
  if(config.claude) {
    let original;try{original=await fs.readFile(config.claude.path,'utf8');}catch(e){if(e.code!=='ENOENT')throw e;}
    outputs.push([config.claude.path,claudeSettings(state.catalog,original?JSON.parse(original):{},config.claude.replaceBuiltInOptions??true),original]);
  }
  if(config.opencode?.format === 'v2') {
    const original=await fs.readFile(config.opencode.path,'utf8');
    outputs.push([config.opencode.path,openCodeV2Config(state.catalog,JSON.parse(original),config.inferenceBase),original]);
  } else if(config.opencode) outputs.push([config.opencode.path,openCodeConfig(state.catalog,config.inferenceBase,config.opencode.defaultModel)]);
  const changed=[];
  for(const [file,doc,expected] of outputs) {
    if(dryRun)changed.push(file);
    else if(await write(file,doc,expected))changed.push(file);
  }
  if(!dryRun)await write(config.statusPath,{revision:state.catalog.revision,checkedAt:state.checkedAt,models:state.catalog.models.map(m=>m.id),targets:outputs.map(([file])=>file),source:state.source},undefined,false);
  return {state,changed,retained:false};
}
async function main() {
  const config=await read(configPath);
  const [command='sync',...args]=process.argv.slice(2);
  if(command==='status') {
    console.log(JSON.stringify(await read(config.statusPath),null,2));return;
  }
  const lock=path.join(path.dirname(configPath),'.sync-lock');
  await fs.mkdir(path.dirname(lock),{recursive:true,mode:0o700});
  // OS advisory locking avoids stale lockfiles after interrupted launches.
  if(!process.env.CPA_SYNC_LOCKED && command==='sync') {
    const result=spawnSync('flock',['-w','8',lock,process.execPath,fileURLToPath(import.meta.url),...process.argv.slice(2)],
      {stdio:'inherit',env:{...process.env,CPA_SYNC_LOCKED:'1'}});
    if(result.error)throw new Error('flock is required for automatic synchronization');
    process.exitCode=result.status??1;return;
  }
  if(command==='sync') {
    const result=await synchronize(config,{offline:args.includes('--offline'),dryRun:args.includes('--dry-run')});
    if(!args.includes('--quiet'))console.log(JSON.stringify({source:result.state.source,revision:result.state.catalog.revision,models:result.state.catalog.models.length,changed:result.changed,retained:result.retained}));
    return;
  }
  if(command==='list') {
    const result=await loadCatalog({config:{url:config.url,profile:config.profile},apiKey:await key(),cacheDir:config.cacheDir,seedFile:config.seedFile??path.join(root,'seed.json')});
    for(const m of result.catalog.models)console.log(`${m.id}\t${m.name}\t${m.context_window} context\t${efforts(m).join('/')}`);
    return;
  }
  if(command==='claude' || command==='opencode') {
    // Reuse the locked synchronizer before launch; its bounded fallback leaves valid files intact.
    const refresh=spawnSync(process.execPath,[fileURLToPath(import.meta.url),'sync','--quiet'],{stdio:'inherit',env:process.env});
    if(refresh.status!==0)console.error('CPA refresh failed; launching with installed settings.');
    const apiKey=await key();
    if(!apiKey)throw new Error('CPA_API_KEY is unavailable');
    const env={...process.env,CPA_API_KEY:apiKey};
    let executable=command==='opencode' ? config.opencode.executable??command : command;let launchArgs=args;
    if(command==='opencode' && path.isAbsolute(executable))env.PATH=path.dirname(executable)+path.delimiter+(env.PATH??'');
    if(command==='claude') {
      const state=await loadCatalog({config:{url:config.url,profile:config.profile},apiKey,cacheDir:config.cacheDir,seedFile:config.seedFile??path.join(root,'seed.json')});
      const id=args[0];const model=state.catalog.models.find(m=>m.id===id);
      if(!model)throw new Error('Choose a catalog model: cpa-models list; cpa-models claude MODEL [Claude arguments]');
      const rest=args.slice(1);
      if(rest.some(x=>x==='--model'||x.startsWith('--model=')||x==='--fallback-model'||x.startsWith('--fallback-model=')))throw new Error('Use the launcher model argument; model/fallback overrides would invalidate its context limits');
      env.ANTHROPIC_BASE_URL=config.inferenceBase;
      env.ANTHROPIC_API_KEY=apiKey;
      delete env.ANTHROPIC_AUTH_TOKEN;
      delete env.CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT;
      env.CLAUDE_CODE_MAX_CONTEXT_TOKENS=String(model.context_window);
      env.CLAUDE_CODE_AUTO_COMPACT_WINDOW=String(Math.min(model.context_window,Math.max(8192,model.context_window-Math.min(model.max_output_tokens,32000))));
      env.CCSTATUSLINE_CONTEXT_SIZE_FALLBACK=String(model.context_window);
      env.CLAUDE_CODE_MAX_OUTPUT_TOKENS=String(Math.min(model.max_output_tokens,32000));
      launchArgs=['--settings',config.claude.path,'--model',id,...rest];
      console.error(`CPA: ${id}, ${model.context_window} context. Relaunch with another model to update Claude's session-wide limits.`);
    } else env.OPENCODE_CONFIG=config.opencode.path;
    const child=spawn(executable,launchArgs,{stdio:'inherit',env});
    child.on('error',()=>{console.error(`${executable} is not installed or could not start.`);process.exitCode=1;});
    child.on('exit',(code,signal)=>{process.exitCode=code??(signal?1:0);});
    return;
  }
  throw new Error('Commands: sync [--dry-run|--offline], status, list, claude MODEL [args], opencode [args]');
}
main().catch(error=>{console.error(`CPA catalog: ${error.message}`);process.exitCode=1;});
