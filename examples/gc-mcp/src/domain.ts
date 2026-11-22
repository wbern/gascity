// Deliberately smaller than the API schemas: no force, reassign, config or destructive actions.
import {z} from 'zod';
import type {components} from './api-types.js';
const text=z.string().max(4000).refine(v=>!/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(v));
const name=z.string().min(1).max(128).regex(/^[A-Za-z0-9][A-Za-z0-9._:/-]*$/).refine(v=>!v.split('/').some(p=>p==='..'||p==='.'));
export const DOMAIN_SCHEMAS={
 mail_send:z.strictObject({to:name,subject:text.min(1),body:text.optional(),rig:name.optional()}),
 mail_reply:z.strictObject({body:text.min(1),subject:text.optional()}),
 mail_mark_read:z.strictObject({}),
 bead_create:z.strictObject({title:text.min(1),description:text.optional(),rig:name,type:z.enum(['task','bug','feature','epic']).optional(),priority:z.number().int().min(0).max(4).optional(),labels:z.array(name).max(20).optional(),parent:name.optional()}),
 bead_sling:z.strictObject({target:name,bead:name,rig:name,formula:name.optional()}),
} as const;
export type DomainOp=keyof typeof DOMAIN_SCHEMAS;
// These assignments bind the selected writable subset to the generated API contract.
type Bodies=components['schemas'];
type Check<T extends U,U>=T;
type _Send=Check<z.output<typeof DOMAIN_SCHEMAS.mail_send>,Bodies['MailSendInputBody']>;
type _Reply=Check<z.output<typeof DOMAIN_SCHEMAS.mail_reply>,Bodies['MailReplyInputBody']>;
type _Create=Check<z.output<typeof DOMAIN_SCHEMAS.bead_create>,Bodies['BeadCreateInputBody']>;
type _Sling=Check<z.output<typeof DOMAIN_SCHEMAS.bead_sling>,Bodies['SlingInputBody']>;
