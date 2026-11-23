// Shared private metadata IO; one server process owns each file.
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
export class ReplyStateError extends Error {}
export function loadPrivateJson(file: string): unknown | undefined {
 let fd: number | undefined;
 try {
  if (!fs.lstatSync(file).isFile()) throw Error("not a regular file");
  fd=fs.openSync(file,fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW|fs.constants.O_NONBLOCK);
  const stat=fs.fstatSync(fd);
  if(!stat.isFile()||(stat.mode&0o077)!==0||(process.getuid&&stat.uid!==process.getuid())||stat.size>1_000_000)throw Error("invalid file");
  return JSON.parse(fs.readFileSync(fd,"utf8"));
 }catch(e){
  if((e as NodeJS.ErrnoException).code==='ENOENT')return undefined;
  throw new ReplyStateError("Invalid reply state; refusing to discard continuity metadata");
 }finally{if(fd!==undefined)fs.closeSync(fd);}
}
export function savePrivateJson(file:string,value:unknown):void{
 const temp=`${file}.${process.pid}.${crypto.randomUUID()}.tmp`;
 let fd:number|undefined;
 try{
  fs.mkdirSync(path.dirname(file),{recursive:true,mode:0o700});
  fd=fs.openSync(temp,fs.constants.O_CREAT|fs.constants.O_EXCL|fs.constants.O_WRONLY,0o600);
  fs.writeFileSync(fd,JSON.stringify(value));fs.fsyncSync(fd);fs.closeSync(fd);fd=undefined;
  fs.renameSync(temp,file);
  const directory=fs.openSync(path.dirname(file),fs.constants.O_RDONLY);
  try{fs.fsyncSync(directory);}finally{fs.closeSync(directory);}
 }catch{throw new ReplyStateError("Reply state could not be saved; automatic continuation is disabled");}
 finally{if(fd!==undefined)fs.closeSync(fd);try{fs.unlinkSync(temp);}catch{/* rename consumed it, or creation failed */}}
}
