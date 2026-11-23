#!/usr/bin/env python3
"""Author-side fixed public policy verification. No runtime config/prompt projection."""
import argparse, datetime, hashlib, json, pathlib, re, socket, subprocess, tomllib
parser=argparse.ArgumentParser()
parser.add_argument('--infra-root',required=True);parser.add_argument('--city-root',required=True);parser.add_argument('--check',action='store_true')
a=parser.parse_args();base=pathlib.Path(__file__).resolve().parent.parent;infra=pathlib.Path(a.infra_root).resolve();city=pathlib.Path(a.city_root).resolve()
definition_raw=(base/'guide/city-policy.source.json').read_bytes();definition=json.loads(definition_raw)
if socket.gethostname().split('.')[0]!=definition['host']:raise SystemExit('Wrong policy verification host')
def run(args):return subprocess.check_output(args,text=False,timeout=15)
def digest(raw):return hashlib.sha256(raw).hexdigest()
def norm(text):return re.sub(r'\s+',' ',re.sub(r'[*`]','',text)).lower()
def source(rel,installed=None,require_match=True):
 revision=run(['git','-C',str(infra),'log','-1','--format=%H','--',rel]).decode().strip()
 if not re.fullmatch('[0-9a-f]{40}',revision):raise SystemExit('Missing source revision')
 committed=run(['git','-C',str(infra),'show',revision+':'+rel]);working=(infra/rel).read_bytes();live=installed.read_bytes() if installed else working
 matches=committed==working==live
 if require_match and not matches:raise SystemExit('Public policy source drift: '+rel)
 return {'repository':'wbern/gas-city-infra','path':rel,'revision':revision,'sha256':digest(committed)},committed,{'committed_working_match':committed==working,'committed_installed_match':committed==live,'working_installed_match':working==live,'installed_sha256':digest(live),'working_sha256':digest(working)}
dispatch_path='city-template/skills/gc-dispatch/SKILL.md';review_path='city-template/packs/gas-city-basics/skills/gas-city-pr-review/SKILL.md'
ds,dispatch,_=source(dispatch_path,city/'.gc/workspaces/gc-architect/.agents/skills/gc-dispatch/SKILL.md')
rs,review,_=source(review_path,city/'.gc/workspaces/gc-architect/.agents/skills/basics.gas-city-pr-review/SKILL.md')
for key,text in [('dispatch_rules',dispatch),('review_rules',review)]:
 for clause in definition[key]:
  if norm(clause['anchor']) not in norm(text.decode()):raise SystemExit('Unverified curated policy clause')
ms,manifest,_=source('config/review-lane-ownership.toml')
lanes=[]
for row in tomllib.loads(manifest.decode())['lanes']:
 if row['owner_city']!='gc3':raise SystemExit('Review ownership moved; manual policy review required')
 lanes.append({k:row[k] for k in ['repository','review_context','owner_city','owner_order']})
if len({(r['repository'],r['review_context']) for r in lanes})!=len(lanes)or len(lanes)>8:raise SystemExit('Ambiguous/unbounded review lanes')
orders=json.loads(run(['gc','--city',str(city),'order','list','--all','--json']))
if not orders['ok']or orders['city_name']!=definition['city']or orders['summary']['filter']!='all'or len(orders['orders'])!=orders['summary']['total']:raise SystemExit('Incomplete/wrong-city order evidence')
if not {r['owner_order']for r in lanes}<={r['name']for r in orders['orders']}:raise SystemExit('Owning order not resolved on execution city')
ss,_,selector_state=source('scripts/worker-level-sling.py',require_match=False)
ws,_,routing_state=source('city-template/config/worker-level-routing-v1.toml',city/'config/worker-level-routing-v1.toml',require_match=False)
# Hash drift is explicit evidence, never permission to teach an uncommitted provider default.
result={k:definition[k]for k in ['id','profile','host','city','label','version']}
result.update(definition_sha256=digest(definition_raw),verified_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),dispatch_rules=[r['text']for r in definition['dispatch_rules']],review_rules=[r['text']for r in definition['review_rules']],review_lanes=lanes,sources={'dispatch_skill':ds,'review_skill':rs,'review_manifest':ms,'selector':ss,'routing':ws},dispatch_verification={'selector':selector_state,'routing':routing_state},provider_defaults_verified=selector_state['committed_installed_match']and selector_state['committed_working_match']and routing_state['committed_installed_match']and routing_state['committed_working_match'])
target=base/'src/city-policy-data.ts';prefix='// Generated author-verified public city policy snapshot; no raw configuration or prompts.\nexport const CITY_POLICY = '
if a.check:
 old=target.read_text();previous=json.loads(old[len(prefix):].removesuffix(' as const;\n'));result['verified_at']=previous['verified_at']
 if old!=prefix+json.dumps(result,indent=2)+' as const;\n':raise SystemExit('City policy snapshot drifted; review before regenerating')
else:target.write_text(prefix+json.dumps(result,indent=2)+' as const;\n')
print('Verified bounded city policy:',result['label'],len(lanes),'review lanes; provider defaults verified:',result['provider_defaults_verified'])
