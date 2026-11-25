import {CITY_POLICY} from './city-policy-data.js';
import {ROLE_PROFILE} from './role-contracts-data.js';
const verified=(profile:string|undefined)=>profile===CITY_POLICY.profile&&profile===ROLE_PROFILE.id&&CITY_POLICY.host===ROLE_PROFILE.host&&CITY_POLICY.city===ROLE_PROFILE.city;
export function connectionIdentity(profile:string|undefined){
 return verified(profile)?{verified:true,operator_city:CITY_POLICY.label,connector_host:CITY_POLICY.host,api_city:CITY_POLICY.city,profile:CITY_POLICY.profile,basis:'deployment host/city profile checked at startup'}:{verified:false,basis:'no matching verified deployment profile'};
}
export function cityPolicy(profile:string|undefined,repository?:string,context?:string){
 if(!verified(profile))return undefined;
 const lanes=CITY_POLICY.review_lanes.filter(r=>(!repository||r.repository===repository.toLowerCase())&&(!context||r.review_context===context));
 return {origin:'installed_city_policy_snapshot',version:CITY_POLICY.version,verified_at:CITY_POLICY.verified_at,
  scope:'snapshot of verified city rules; current holds, readiness and concrete custody require fresh reads',
  dispatch:{selector:'worker-level-sling.py',rules:CITY_POLICY.dispatch_rules,
   provider_defaults:CITY_POLICY.provider_defaults_verified?'source_files_match_installed_snapshot':'unverified_installed_source_drift',
   selector_matches_commit:CITY_POLICY.dispatch_verification.selector.committed_installed_match,
   routing_matches_commit:CITY_POLICY.dispatch_verification.routing.committed_installed_match,
   transport_boundary:'core bead_sling transport does not execute the city worker-level selector'},
  review:{status:lanes.length?'known_repository_context':'unknown_repository_context',lanes,rules:CITY_POLICY.review_rules},
  sources:[CITY_POLICY.sources.dispatch_skill,CITY_POLICY.sources.review_skill,CITY_POLICY.sources.review_manifest]};
}
