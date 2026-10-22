// node docs/exacq/lab-soak.mjs OUTPUT_DIR [http://127.0.0.1:11984]
// Requires configured exacq_q, exacq_axis, exacq_dahua sources and FFmpeg.
// No credentials or source URLs are printed or saved by this runner.
import fs from 'node:fs/promises';
import {spawn} from 'node:child_process';
import assert from 'node:assert/strict';
const [dir,api='http://127.0.0.1:11984']=process.argv.slice(2);
assert(dir,'output directory required');
await fs.mkdir(dir,{recursive:true,mode:0o700});
const results={startedAt:new Date().toISOString(),captures:[]};
function run(cmd,args){return new Promise((resolve,reject)=>{const p=spawn(cmd,args),out=[],err=[];p.stdout.on('data',x=>out.push(x));p.stderr.on('data',x=>err.push(x));p.on('error',reject);p.on('close',code=>resolve({code,out:Buffer.concat(out).toString(),err:Buffer.concat(err).toString()}));});}
async function capture(name){
 const path=`${dir}/${name}.mp4`,file=await fs.open(path,'w',0o600),begin=performance.now();
 let firstByteMs,bytes=0,chunks=0,maxReceiveGapMs=0,last;
 try{
  const r=await fetch(`${api}/api/stream.mp4?src=${name}&duration=120`,{signal:AbortSignal.timeout(140000)});
  assert(r.ok,`HTTP ${r.status}`);
  for await(const value of r.body){const now=performance.now();firstByteMs??=now-begin;if(last)maxReceiveGapMs=Math.max(maxReceiveGapMs,now-last);last=now;bytes+=value.length;chunks++;await file.write(value);}
 }finally{await file.close();}
 assert(bytes>4096,'insufficient media');
 const row={name,path,firstByteMs,bytes,chunks,maxReceiveGapMs,elapsedMs:performance.now()-begin};results.captures.push(row);return row;
}
async function decode(row){
 const p=await run('ffprobe',['-v','error','-count_frames','-count_packets','-select_streams','v:0','-show_entries','stream=codec_name,width,height,nb_read_frames,nb_read_packets:packet=pts_time,dts_time,duration_time','-show_packets','-of','json',row.path]);
 assert.equal(p.code,0,p.err);assert.equal(p.err,'');
 const j=JSON.parse(p.out),s=j.streams[0];assert.equal(s.nb_read_frames,s.nb_read_packets,'frame/packet mismatch');
 const d=await run('ffmpeg',['-v','error','-i',row.path,'-map','0:v:0','-fps_mode','passthrough','-enc_time_base','1:90000','-f','null','-']);assert.equal(d.code,0,d.err);assert.equal(d.err,'');
 const packets=j.packets,pts=packets.map(x=>+x.pts_time),dt=pts.slice(1).map((x,i)=>x-pts[i]);
 for(let i=1;i<packets.length;i++)assert(+packets[i].dts_time>=+packets[i-1].dts_time+ +(packets[i-1].duration_time??0)-0.00002,'sample overlap');
 Object.assign(row,s,{mediaSeconds:pts.at(-1)-pts[0],minimumIntervalMs:Math.min(...dt)*1000,maximumIntervalMs:Math.max(...dt)*1000,decodeErrors:0});
}
try{
 const rows=await Promise.all(['exacq_q','exacq_axis','exacq_dahua'].map(capture));
 console.log('Three 120-second native captures complete; decoding.');
 for(const row of rows)await decode(row);
 results.ok=true;results.completedAt=new Date().toISOString();
 console.log(JSON.stringify(results));
}catch(e){results.ok=false;results.error=String(e);console.error(String(e));process.exitCode=1;}
finally{await fs.writeFile(`${dir}/metrics.json`,JSON.stringify(results,null,2),{mode:0o600});}
