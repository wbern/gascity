import {ROLE_PROFILE} from './role-contracts-data.js';
export function resolveGuideProfile(value:string|undefined,city:string,host?:string):string|undefined{
 if(!value)return undefined;
 if(value!==ROLE_PROFILE.id||city!==ROLE_PROFILE.city||host!==ROLE_PROFILE.host)throw Error('GC_MCP_GUIDE_PROFILE does not match a verified profile, city and host');
 return value;
}
export function roleFor(profile:string|undefined,agent:Record<string,unknown>){
 if(profile!==ROLE_PROFILE.id||agent.pack_derived!==true)return undefined;
 return ROLE_PROFILE.contracts.find(c=>c.identity.pool?agent.pool===c.identity.pool&&agent.pack===c.identity.pack:agent.name===c.identity.agent&&(agent.pool===undefined||agent.pool==='')&&(c.identity.pack?agent.pack===c.identity.pack:(agent.pack===undefined||agent.pack==='')));
}
export function publicRole(contract:NonNullable<ReturnType<typeof roleFor>>){return {
 id:contract.id,purpose:contract.purpose,standards:contract.standards,verification:contract.verification,sources:contract.sources,
 verified_host:ROLE_PROFILE.host,verified_city:ROLE_PROFILE.city,profile:ROLE_PROFILE.id,version:ROLE_PROFILE.version,verified_at:ROLE_PROFILE.verified_at,
 scope:'default role standards snapshot; current assignment and existing human authority are separate',
};}

export function publicRoleCatalog(contracts:NonNullable<ReturnType<typeof roleFor>>[]){return {
 profile:ROLE_PROFILE.id,version:ROLE_PROFILE.version,verified_at:ROLE_PROFILE.verified_at,total:contracts.length,
 contracts:contracts.slice(0,6).map(c=>({id:c.id,purpose:c.purpose,target:c.identity.pool??c.identity.agent})),
 omitted:Math.max(0,contracts.length-6),
};}
