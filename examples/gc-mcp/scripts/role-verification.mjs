// Author-side verification only. No raw resolved config or installed prompt is returned.
import crypto from 'node:crypto';
export const digest=text=>crypto.createHash('sha256').update(text).digest('hex');
const normalize=text=>text.replace(/[*`]/g,'').replace(/\s+/g,' ').toLowerCase();
export function publicBinding(text){
 const fields={};
 for(const line of text.replace(/\x1b\[[0-9;]*m/g,'').split('\n')){
  const m=/^\s*(name|dir|prompt_template)\s*=\s*(.*?)\s+#/.exec(line);
  if(m){if(Object.hasOwn(fields,m[1]))throw Error('ambiguous configuration projection');fields[m[1]]=m[2].trim();}
 }
 return fields;
}
export function verifyRole(def,fields,sourceText,installedText){
 if(fields.name!==def.selector.agent||fields.dir!==def.selector.rig||fields.prompt_template!==def.template)throw Error('resolved role binding does not match the declared source');
 for(const clause of def.standards)if(!normalize(sourceText).includes(normalize(clause.anchor))||!normalize(installedText).includes(normalize(clause.anchor)))throw Error('a curated standards clause is not verified in both templates');
 return {public_fields_verified:true,verified_clause_count:def.standards.length,full_template_match:digest(sourceText)===digest(installedText),installed_template_sha256:digest(installedText),basis:'selected resolved template and verified clauses; not full effective instructions'};
}
