#!/usr/bin/env python3
"""Install CPA harness synchronization on an existing Linux CPA workstation."""
import argparse,datetime,hashlib,json,os,shutil,subprocess,tomllib
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('--claude-user-settings',action='store_true');p.add_argument('--enroll-pi',action='store_true');p.add_argument('--fix-server-codex-url',action='store_true');args=p.parse_args()
h=Path.home();src=Path(__file__).resolve().parent
root=h/'.local/lib/cpa-harness-sync';configdir=h/'.config/cpa-catalog'
backup=h/'.local/state/cpa-catalog/install-backups'/datetime.datetime.now().strftime('%Y%m%dT%H%M%S')
backup.mkdir(parents=True,mode=0o700)
manifest=[]
def put(path,data,mode=0o600):
 path=Path(path);raw=data if isinstance(data,bytes) else data.encode()
 before=path.read_bytes() if path.exists() else None
 if before==raw:return
 if before is not None:
  name=hashlib.sha256(str(path).encode()).hexdigest()+'.bak';(backup/name).write_bytes(before)
  manifest.append({'path':str(path),'backup':str(backup/name),'before_sha256':hashlib.sha256(before).hexdigest(),'installed_sha256':hashlib.sha256(raw).hexdigest()})
 else:manifest.append({'path':str(path),'backup':None,'installed_sha256':hashlib.sha256(raw).hexdigest()})
 path.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
 tmp=path.with_name(path.name+'.cpa-install-tmp');tmp.write_bytes(raw);tmp.chmod(mode)
 if (path.read_bytes() if path.exists() else None)!=before:tmp.unlink();raise RuntimeError('Concurrent edit: '+str(path))
 os.replace(tmp,path)
def putjson(path,data):put(path,json.dumps(data,indent=2)+'\n')
node=shutil.which('node');assert node,'Node.js 22+ required'
assert shutil.which('flock'),'flock required'
configdir.mkdir(parents=True,exist_ok=True,mode=0o700)
for name in ['client.mjs','seed.json','adapters.mjs','sync.mjs']:put(root/name,(src/name).read_bytes())
config={'url':'https://serverseesghosts.tail74ed91.ts.net/cpa-catalog/v1/catalog/pi','profile':'personal','inferenceBase':'http://127.0.0.1:8317','cacheDir':str(h/'.cache/cpa-catalog'),'statusPath':str(h/'.local/state/cpa-catalog/status.json')}
seed=json.loads((src/'seed.json').read_text());ids={m['id'] for m in seed['models']}
codex=h/'.codex/config.toml'
if codex.exists():
 c=tomllib.loads(codex.read_text())
 if c.get('model_provider')=='cpa' and c.get('model_catalog_json'):
  target=Path(c['model_catalog_json']).expanduser();templates=json.loads(target.read_text())
  template_path=configdir/'codex-templates.json'
  if not template_path.exists():putjson(template_path,templates)
  local=[m['slug'] for m in templates['models'] if m['slug'].startswith('ninfer') or (m['slug']==c.get('model') and m['slug'] not in ids)]
  config['codex']={'path':str(target),'templates':str(template_path),'localModels':local}
  if args.fix_server_codex_url:
   text=codex.read_text();old='https://kidsseeghosts.tail74ed91.ts.net:8317/v1'
   assert c['model_providers']['cpa']['base_url']==old,'Unexpected provider URL; inspect before editing'
   put(codex,text.replace(old,'http://127.0.0.1:8317/v1'))
claude=h/'.claude/settings.json'
if claude.exists():
 config['claude']={'path':str(claude if args.claude_user_settings else h/'.claude/cpa-catalog.settings.json'),'replaceBuiltInOptions':True}
# OpenCode export is isolated because installed clients may use different config generations.
old_oc=h/'.config/opencode/opencode.jsonc';oc_default=None
if old_oc.exists():
 try:oc_default=json.loads(old_oc.read_text()).get('model')
 except json.JSONDecodeError:pass
config['opencode']={'path':str(configdir/'opencode.json'),'defaultModel':oc_default}
oc_binary=shutil.which('opencode') or str(h/'.opencode/bin/opencode')
if old_oc.exists() and Path(oc_binary).exists():
 version=subprocess.run([oc_binary,'--version'],capture_output=True,text=True,timeout=5).stdout.strip()
 if version.startswith('opencode v2.'):
  config['opencode'].update(path=str(old_oc),format='v2',executable=oc_binary)
putjson(configdir/'sync.json',config)
launcher='#!/usr/bin/env python3\nimport os\nos.execv('+repr(node)+', ['+repr(node)+', '+repr(str(root/'sync.mjs'))+'] + __import__("sys").argv[1:])\n'
put(h/'.local/bin/cpa-models',launcher,0o755)
helper=h/'.local/bin/sync-claude-pi-models'
if helper.exists():
 put(helper,'#!/usr/bin/env python3\n"""Compatibility entrypoint; CPA catalog is now the source."""\nimport os,sys\nargs=[a for a in sys.argv[1:] if a in ("--offline","--dry-run")]\nos.execv('+repr(node)+', ['+repr(node)+', '+repr(str(root/'sync.mjs'))+', "sync", "--quiet"]+args)\n',0o755)
fish=h/'.config/fish/functions/claude-cpa.fish'
if fish.exists() or (h/'.config/fish').exists():
 put(fish,'''function claude-cpa --description "Launch a centrally configured CPA model in Claude Code"
    if test (count $argv) -gt 0
        ~/.local/bin/cpa-models claude $argv
        return
    end
    set -l models (~/.local/bin/cpa-models list)
    if type -q fzf
        set -l choice (printf "%s\\n" $models | fzf --height=22 --reverse --prompt="CPA model > ")
        if test -n "$choice"
            set -l selected (string split -f 1 \\t -- "$choice")
            ~/.local/bin/cpa-models claude "$selected"
        end
    else
        printf "%s\\n" $models
        echo "Usage: claude-cpa MODEL [Claude arguments]"
    end
end
''')
if args.enroll_pi:
 ext=h/'.pi/agent/extensions/cpa-provider.ts';s=ext.read_text()
 assert 'loadCatalog' not in s,'Pi is already enrolled'
 start=s.index('    models: [',s.index('export default function'))
 end=s.rindex('\n  });')
 s=s[:start]+'    models: toPiModels(catalogState.catalog, CPA_BASE),\n'+s[end:]
 s='import { loadCatalog, toPiModels } from "../lib/cpa-catalog/client.mjs";\n'+s
 s=s.replace('export default function recoveredCpaProvider(pi) {','''export default async function recoveredCpaProvider(pi) {
  const catalogState = await loadCatalog();
  const summary = `CPA catalog: ${catalogState.catalog.models.length} models, ${catalogState.source}, revision ${catalogState.catalog.revision.slice(0,12)}.`;
  pi.on("session_start", (_event, ctx) => { if (catalogState.warning) ctx.ui.notify(`${summary} ${catalogState.warning}`, "warning"); });
  pi.registerCommand("cpa-catalog", { description: "Show the central CPA catalog status", handler: async (_args,ctx) => {
    ctx.ui.notify(`${summary} Start a fresh Pi process to update model additions/removals.`, catalogState.warning ? "warning" : "info");
  }});''')
 for name in ['client.mjs','seed.json']:put(h/'.pi/agent/lib/cpa-catalog'/name,(src/name).read_bytes())
 putjson(h/'.pi/agent/cpa-catalog.json',{'url':config['url'],'profile':config['profile']})
 settings=h/'.pi/agent/settings.json';d=json.loads(settings.read_text());d['enabledModels']=['cpa/**']+[x for x in d.get('enabledModels',[]) if not x.startswith('cpa/')]
 put(ext,s);putjson(settings,d)
units=h/'.config/systemd/user'
put(units/'cpa-catalog-sync.service',f'''[Unit]
Description=Synchronize CPA model definitions into local harness catalogs
After=network-online.target

[Service]
Type=oneshot
ExecStart={node} {root}/sync.mjs sync --quiet
TimeoutStartSec=25
NoNewPrivileges=true
UMask=0077
''')
put(units/'cpa-catalog-sync.timer','''[Unit]
Description=Refresh CPA harness catalogs every minute

[Timer]
OnStartupSec=15
OnUnitInactiveSec=60
AccuracySec=5
Unit=cpa-catalog-sync.service

[Install]
WantedBy=timers.target
''')
putjson(backup/'manifest.json',manifest)
result=subprocess.run([node,str(root/'sync.mjs'),'sync'],check=True)
subprocess.run(['systemctl','--user','daemon-reload'],check=True)
subprocess.run(['systemctl','--user','enable','--now','cpa-catalog-sync.timer'],check=True)
print(json.dumps({'config':str(configdir/'sync.json'),'backup':str(backup/'manifest.json'),'targets':list(k for k in ['codex','claude','opencode'] if k in config),'pi_enrolled':args.enroll_pi}))
