// Explicit lab fault injection: only the private relay opened by this process
// is interrupted. Recorder configuration and other viewers are untouched.
// node docs/exacq/lab-reconnect.mjs OUTPUT_DIR LAB_ITEM_JSON API_AUTH_JSON
import fs from 'node:fs/promises';
import net from 'node:net';
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
const [dir,labFile,authFile]=process.argv.slice(2),api='http://127.0.0.1:11984';
const item=JSON.parse(await fs.readFile(labFile,'utf8')),auth=JSON.parse(await fs.readFile(authFile,'utf8'));
const fields=Object.fromEntries(item.fields.map(x=>[x.id,x.value]));
const headers={Authorization:'Basic '+Buffer.from(`${auth.username}:${auth.password}`).toString('base64')};
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const result={captures:[]},sockets=new Set();let down=false,accepted=0;
const relay=net.createServer(client=>{
 if(down){client.destroy();return;}
 accepted++;const upstream=net.connect(22610,'192.168.3.232');
 sockets.add(client);sockets.add(upstream);
 for(const s of [client,upstream]){s.on('error',()=>{client.destroy();upstream.destroy()});s.on('close',()=>{sockets.delete(s);client.destroy();upstream.destroy()});}
 client.pipe(upstream);upstream.pipe(client);
});
await new Promise((resolve,reject)=>{relay.once('error',reject);relay.listen(22649,'127.0.0.1',resolve)});
const source=new URL('exacq://127.0.0.1:22649/459264');source.username=encodeURIComponent(fields.username);source.password=encodeURIComponent(fields.password);
const name='exacq_native_reconnect';
async function capture(label,seconds,inject=false){
 const file=await fs.open(`${dir}/${label}.mp4`,'w',0o600);let first,last,timer,trigger,bytes=0,gap=0,after=0;
 const start=Date.now(),abort=new AbortController();let ended=false;
 timer=setTimeout(()=>abort.abort(),15000);
 try{
  const response=await fetch(`${api}/api/stream.mp4?src=${name}`,{headers,signal:abort.signal});assert(response.ok,`HTTP${response.status}`);
  for await(const data of response.body){
   const now=Date.now();if(!first){first=now;clearTimeout(timer);timer=setTimeout(()=>abort.abort(),seconds*1000);
    if(inject)trigger=(async()=>{await sleep(5000);down=true;result.outageStarted=Date.now();for(const s of sockets)s.destroy();await sleep(1600);down=false;result.outageRestored=Date.now();})();
   }
   if(last)gap=Math.max(gap,now-last);last=now;bytes+=data.length;if(result.outageRestored&&now>result.outageRestored)after+=data.length;await file.write(data);
  }ended=true;
 }catch(e){if(e.name!=='AbortError')throw e;}finally{clearTimeout(timer);abort.abort();await file.close();if(trigger)await trigger;}
 assert(first,'no startup media');assert(!ended,'liveviewer ended');
 if(inject)assert(after>4096,'no recovered media');
 const row={label,firstByteMs:first-start,bytes,maxReceiveGapMs:gap,bytesAfterRestore:after};result.captures.push(row);return row;
}
async function trimAndDecode(label){
 const path=`${dir}/${label}.mp4`,data=await fs.readFile(path);let pos=0,safe=0;
 while(pos+8<=data.length){const n=data.readUInt32BE(pos);if(n<8||pos+n>data.length)break;const type=data.toString('ascii',pos+4,pos+8);pos+=n;if(type==='mdat')safe=pos;}
 assert(safe>0);await fs.truncate(path,safe);
 const run=(cmd,args)=>new Promise((resolve,reject)=>{const p=spawn(cmd,args),out=[],err=[];p.stdout.on('data',x=>out.push(x));p.stderr.on('data',x=>err.push(x));p.on('error',reject);p.on('close',code=>resolve({code,out:Buffer.concat(out).toString(),err:Buffer.concat(err).toString()}));});
 const p=await run('ffprobe',['-v','error','-count_frames','-count_packets','-select_streams','v:0','-show_entries','stream=nb_read_frames,nb_read_packets','-of','json',path]);assert.equal(p.code,0);assert.equal(p.err,'');const s=JSON.parse(p.out).streams[0];assert.equal(s.nb_read_frames,s.nb_read_packets);
 const d=await run('ffmpeg',['-v','error','-i',path,'-map','0:v:0','-fps_mode','passthrough','-enc_time_base','1:90000','-f','null','-']);assert.equal(d.code,0);assert.equal(d.err,'');Object.assign(result.captures.find(x=>x.label===label),s,{decodeErrors:0});
}
try{
 await fs.mkdir(dir,{recursive:true,mode:0o700});
 const registered=await fetch(`${api}/api/streams?name=${name}&src=${encodeURIComponent(source)}`,{method:'PATCH',headers});assert(registered.ok);
 await capture('outage',24,true);await sleep(1000);await capture('reopen',5);
 const [a,b]=await Promise.all([capture('viewer-cancel',4),capture('viewer-continue',10)]);assert(b.bytes>a.bytes,'remaining viewer stopped with cancelled viewer');
 for(const row of result.captures)await trimAndDecode(row.label);
 result.acceptedConnections=accepted;assert(accepted>=3,'upstream not reopened');result.ok=true;console.log(JSON.stringify(result));
}catch(e){result.ok=false;result.error=String(e);console.error(String(e));process.exitCode=1;}
finally{down=true;for(const s of sockets)s.destroy();await new Promise(r=>relay.close(r));await fs.writeFile(`${dir}/metrics.json`,JSON.stringify(result,null,2),{mode:0o600});}
