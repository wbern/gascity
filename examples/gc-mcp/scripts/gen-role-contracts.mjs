// Opt-in, operator-verified snapshot for the named deployment. Fixed declared template inputs only;
// never ship raw prompts, anchors, resolved config, environment fields or runtime file access.
import fs from 'node:fs';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {digest,publicBinding,verifyRole} from './role-verification.mjs';
const arg=name=>{const i=process.argv.indexOf('--'+name);return i>=0?process.argv[i+1]:undefined;};
const infra=arg('infra-root'),city=arg('city-root');if(!infra||!city)throw Error('--infra-root and --city-root are required');
const definition=fs.readFileSync(new URL('../guide/role-contracts.source.json',import.meta.url),'utf8');
const input=JSON.parse(definition);
const host=spawnSync('hostname',['-s'],{encoding:'utf8'});if(host.status!==0||host.stdout.trim()!==input.host)throw Error('wrong verification host');
const response=await fetch(`http://127.0.0.1:8372/v0/city/${encodeURIComponent(input.city)}/agents`,{redirect:'error',signal:AbortSignal.timeout(8000)});
if(!response.ok)throw Error('agent directory could not be verified');const directory=await response.json();
if(!Array.isArray(directory.items)||directory.partial||directory.next_cursor||directory.total!==directory.items.length)throw Error('incomplete agent directory');
const contracts=[];
for(const def of input.contracts){
 const run=spawnSync('gc',['config','explain','--rig',def.selector.rig,'--agent',def.selector.agent],{cwd:city,encoding:'utf8',timeout:15000});
 if(run.status!==0)throw Error('configuration read failed for '+def.id);
 const fields=publicBinding(run.stdout);
 const sourcePath=path.join(infra,'city-template',def.template),livePath=path.join(city,def.template);
 const source=fs.readFileSync(sourcePath,'utf8'),installed=fs.readFileSync(livePath,'utf8');
 const verification=verifyRole(def,fields,source,installed);
 const matches=directory.items.filter(a=>a.pack_derived===true&&(def.identity.pool?a.pool===def.identity.pool&&a.pack===def.identity.pack:a.name===def.identity.agent&&(a.pool===undefined||a.pool==='')&&(def.identity.pack?a.pack===def.identity.pack:(a.pack===undefined||a.pack===''))));
 if(!matches.length)throw Error('declared configuration identity was not observed for '+def.id);
 const sources=[];
 for(const rel of [def.config,def.template]){
  const repositoryPath='city-template/'+rel;
  const revision=spawnSync('git',['-C',infra,'log','-1','--format=%H','--',repositoryPath],{encoding:'utf8'});
  if(revision.status!==0||!revision.stdout.trim())throw Error('source revision unavailable');
  sources.push({repository:'wbern/gas-city-infra',path:repositoryPath,revision:revision.stdout.trim(),sha256:digest(fs.readFileSync(path.join(infra,repositoryPath)))});
 }
 contracts.push({id:def.id,identity:def.identity,purpose:def.purpose,standards:def.standards.map(x=>x.text),verification,sources});
}
const result={definition_sha256:digest(definition),id:input.id,city:input.city,host:input.host,version:input.version,verified_at:new Date().toISOString(),contracts};
const target=new URL('../src/role-contracts-data.ts',import.meta.url),prefix='// Generated operator-verified standards snapshot; raw prompts and verification anchors are excluded.\nexport const ROLE_PROFILE = ';
if(process.argv.includes('--check')){
 const old=fs.readFileSync(target,'utf8');const previous=JSON.parse(old.slice(prefix.length).replace(/ as const;\s*$/,''));result.verified_at=previous.verified_at;
 if(old!==prefix+JSON.stringify(result,null,2)+' as const;\n')throw Error('role contract snapshot drifted; review before regenerating');
}else fs.writeFileSync(target,prefix+JSON.stringify(result,null,2)+' as const;\n');
console.log('Verified role contracts:',contracts.map(x=>x.id).join(', '));
