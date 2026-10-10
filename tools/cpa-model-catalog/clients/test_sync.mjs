import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import http from 'node:http';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
const dir=await fs.mkdtemp(path.join(os.tmpdir(),'cpa-sync-test-'));
const seed=JSON.parse(await fs.readFile(new URL('./seed.json',import.meta.url),'utf8'));
let served=structuredClone(seed),status=200;
const server=http.createServer((req,res)=>{
 assert.equal(req.headers.authorization,'Bearer test-key');
 res.writeHead(status,{'content-type':'application/json',etag:'"test"'});res.end(JSON.stringify(served));
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const config={url:`http://127.0.0.1:${server.address().port}/v1/catalog/pi`,profile:'personal',inferenceBase:'http://127.0.0.1:8317',cacheDir:path.join(dir,'cache'),seedFile:fileURLToPath(new URL('./seed.json',import.meta.url)),statusPath:path.join(dir,'status.json'),
 codex:{path:path.join(dir,'codex.json'),templates:path.join(dir,'templates.json'),localModels:['local/model']},
 claude:{path:path.join(dir,'claude.json')},opencode:{path:path.join(dir,'opencode.json')}};
await fs.writeFile(config.codex.templates,JSON.stringify({models:[{slug:'gpt-5.6-sol',shell_type:'shell_command',base_instructions:'transport'},{slug:'local/model'}]}));
await fs.writeFile(config.claude.path,JSON.stringify({model:'devin/swe-2',permissions:{allow:['Read']}}));
const configPath=path.join(dir,'sync.json');await fs.writeFile(configPath,JSON.stringify(config));
function run(...args){return new Promise((resolve,reject)=>{
 const child=spawn(process.execPath,[fileURLToPath(new URL('./sync.mjs',import.meta.url)),'sync',...args],{env:{...process.env,HOME:dir,CPA_SYNC_CONFIG:configPath,CPA_API_KEY:'test-key'}});
 let out='',err='';child.stdout.on('data',x=>out+=x);child.stderr.on('data',x=>err+=x);child.on('close',code=>code===0?resolve({out,err}):reject(new Error(err||out)));
});}
const read=async p=>JSON.parse(await fs.readFile(p,'utf8'));
try{
 await run();assert.equal((await read(config.codex.path)).models.length,12);
 assert.equal((await read(config.claude.path)).modelPicker.options.length,11);
 const stat=await fs.stat(config.codex.path);await run();assert.equal((await fs.stat(config.codex.path)).mtimeMs,stat.mtimeMs);
 served.models.push({...structuredClone(seed.models[0]),id:'added-central-model'});served.revision='a'.repeat(64);
 await run();assert.equal((await read(config.claude.path)).modelPicker.options.length,12);
 served=structuredClone(seed);await run();assert.equal((await read(config.claude.path)).modelPicker.options.length,11);
 assert.deepEqual((await read(config.claude.path)).permissions,{allow:['Read']});
 const before=await fs.readFile(config.codex.path,'utf8');status=503;await run();assert.equal(await fs.readFile(config.codex.path,'utf8'),before);
 status=200;served.models[0].pi.headers={Authorization:'malicious'};await run();assert.equal(await fs.readFile(config.codex.path,'utf8'),before);
 await run('--offline');assert.equal(await fs.readFile(config.codex.path,'utf8'),before);
 console.log('Sync integration passed: central add/remove, settings preservation, no-op writes, offline and malformed-response retention.');
}finally{server.closeAllConnections();await new Promise(resolve=>server.close(resolve));await fs.rm(dir,{recursive:true,force:true});}
