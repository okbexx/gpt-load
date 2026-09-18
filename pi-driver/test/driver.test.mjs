import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { once } from 'node:events';
import { zstdDecompressSync } from 'node:zlib';
import { createDriver, normalizeBaseUrl } from '../driver.mjs';

const token = `e30.${Buffer.from(JSON.stringify({'https://api.openai.com/auth': {chatgpt_account_id:'synthetic-account'}})).toString('base64')}.synthetic`;
const secret = 'synthetic-sidecar-secret-at-least-32-chars';
const terminal = {id:'resp_native',object:'response',status:'completed',output:[
  {type:'reasoning',id:'rs',encrypted_content:'signature-native',summary:[]},
  {type:'function_call',id:'fc1',call_id:'c1',name:'one',arguments:'{}'},
  {type:'function_call',id:'fc2',call_id:'c2',name:'two',arguments:'{"nested":true}'}
],usage:{input_tokens:7,output_tokens:3,total_tokens:10,input_tokens_details:{cached_tokens:2},unknown_usage:99},unknown_terminal:{preserve:true}};
const event = x => `data: ${JSON.stringify(x)}\n\n`;
const native = ': keepalive\n\n' + [
  {type:'response.created',response:{id:terminal.id}},
  ...terminal.output.map((item,output_index)=>({type:'response.output_item.added',output_index,item:{...item,arguments:''}})),
  {type:'response.function_call_arguments.delta',output_index:2,delta:'{"nested":'},
  {type:'response.reasoning_summary_text.delta',output_index:0,delta:'thinking 💡'},
  {type:'response.function_call_arguments.delta',output_index:1,delta:'{}'},
  {type:'response.function_call_arguments.delta',output_index:2,delta:'true}'},
  {type:'future.native',signature:'untouched',parallel:['c1','c2']},
  ...terminal.output.map((item,output_index)=>({type:'response.output_item.done',output_index,item})),
  {type:'response.completed',response:terminal},
].map(event).join('');
async function listen(server) { server.listen(0,'127.0.0.1'); await once(server,'listening'); return `http://127.0.0.1:${server.address().port}`; }
async function fixture(t, handler = (_req,res) => {res.writeHead(200,{'content-type':'text/event-stream','x-request-id':'safe-id','set-cookie':'SECRET'});res.end(native);}, options = {}) {
  const requests=[];
  const upstream=http.createServer(async (req,res)=>{const chunks=[]; for await(const x of req) chunks.push(x); let body=Buffer.concat(chunks); if(req.headers['content-encoding']==='zstd') body=zstdDecompressSync(body); requests.push({headers:req.headers,body:JSON.parse(body.toString()),url:req.url}); handler(req,res);});
  const base=await listen(upstream);
  const driver=createDriver({token:secret,allowedBaseUrls:[base],...options});
  const url=await listen(driver);
  t.after(async()=>{driver.closeAllConnections();upstream.closeAllConnections();await Promise.all([new Promise(r=>driver.close(r)),new Promise(r=>upstream.close(r))]);});
  const request={version:1,provider:'codex',format:'openai-response',operation:'responses',model:'native-model',stream:true,credential:{access_token:token,account_id:'synthetic-account'},body:{model:'native-model',input:[{role:'user',content:'hello'}],store:false,parallel_tool_calls:true,reasoning:{effort:'high'},unknown_request:{keep:1}},headers:{},base_url:base,proxy_url:'',session_id:'session-fixture'};
  async function send(overrides={}, auth=secret) {const res=await fetch(url+'/v1/execute',{method:'POST',headers:{authorization:`Bearer ${auth}`,'content-type':'application/json'},body:JSON.stringify({...request,...overrides})});const text=await res.text();return {status:res.status,frames:text.trim().split('\n').map(JSON.parse),text};}
  return {requests,request,send,url,base};
}

test('real Pi dispatch preserves native payload, original SSE and terminal fields',async t=>{
  const f=await fixture(t);const r=await f.send();assert.equal(r.status,200);
  assert.deepEqual(r.frames.at(-1),{type:'done',dispatch_state:'maybe_sent'});
  assert.equal(Buffer.concat(r.frames.filter(x=>x.type==='chunk').map(x=>Buffer.from(x.data,'base64'))).toString(),native);
  assert.equal(r.frames[0].driver,'pi');assert.equal(r.frames[0].headers['set-cookie'],undefined);
  assert.equal(f.requests.length,1);assert.equal(f.requests[0].headers.authorization,`Bearer ${token}`);
  assert.equal(f.requests[0].headers['chatgpt-account-id'],'synthetic-account');assert.equal(f.requests[0].headers.originator,'pi');
  assert.equal(f.requests[0].url,'/codex/responses');assert.deepEqual(f.requests[0].body,{...f.request.body,stream:true});
  const unary=await f.send({stream:false});assert.deepEqual(unary.frames.find(x=>x.type==='result').response,terminal);assert.equal(unary.frames.some(x=>x.type==='chunk'),false);
});

test('valid JWT base64url payloads are accepted',async t=>{
  const f=await fixture(t);
  const payload=Buffer.from(JSON.stringify({'https://api.openai.com/auth':{chatgpt_account_id:'synthetic-account'},extra:'💡💡💡'})).toString('base64url');
  assert.match(payload,/[-_]/);
  const r=await f.send({credential:{access_token:`e30.${payload}.synthetic`,account_id:'synthetic-account'}});
  assert.equal(r.status,200);
  assert.equal(r.frames.at(-1).type,'done');
  assert.equal(f.requests.length,1);
  assert.equal(f.requests[0].headers.authorization,`Bearer e30.${payload}.synthetic`);
});

test('JWT parser compatibility remains request-local and preserves signed bytes', async t => {
  const f = await fixture(t);
  const credentials = ['synthetic-account-a', 'synthetic-account-b'].map((account, index) => {
    const payload = Buffer.from(JSON.stringify({'https://api.openai.com/auth':{chatgpt_account_id:account},extra:'💡💡💡'})).toString('base64url');
    assert.match(payload, /[-_]/);
    const padded = index ? payload + '='.repeat((4 - payload.length % 4) % 4) : payload;
    return {account_id: account, access_token: `e30.${padded}.synthetic-signature-${index}`};
  });
  const results = await Promise.all(credentials.map((credential, index) => f.send({credential, stream: index === 0})));
  for (const result of results) assert.equal(result.frames.at(-1).type, 'done');
  assert.equal(f.requests.length, credentials.length);
  for (const request of f.requests) {
    const credential = credentials.find(c => c.account_id === request.headers['chatgpt-account-id']);
    assert.ok(credential);
    assert.equal(request.headers.authorization, `Bearer ${credential.access_token}`);
  }
});

test('admission rejects unsafe inputs before dispatch',async t=>{
 const f=await fixture(t);
 for(const change of [{provider:'other'},{proxy_url:'http://proxy'},{base_url:'http://127.0.0.1:1'},{base_url:'https://user:pass@chatgpt.com/backend-api'},{headers:{Authorization:'bad'}},{headers:{'chatgpt-account-id':'wrong'}},{headers:{Host:'other'}},{credential:{access_token:token,account_id:'wrong'}},{body:{previous_response_id:'prior'}},{body:{conversation:'prior'}},{body:{store:true}},{body:{background:true}},{body:{model:'different'}},{body:{stream:false}}]) {
  if(change.body) change.body={...f.request.body,...change.body};
  const r=await f.send(change);assert.ok(r.status>=400);assert.equal(r.frames[0].dispatch_state,'not_sent');
 }
 assert.equal((await f.send({},'wrong')).status,401);assert.equal(f.requests.length,0);
});

test('upstream errors sanitized with one attempt and actual status',async t=>{
 const f=await fixture(t,(_req,res)=>{res.writeHead(429);res.end(`secret ${token}`);});const r=await f.send();
 assert.equal(r.frames.at(-1).type,'error');assert.equal(r.frames.at(-1).status,429);assert.equal(r.frames.at(-1).dispatch_state,'maybe_sent');assert.ok(!r.text.includes(token));assert.equal(f.requests.length,1);
});

test('missing terminal is not success',async t=>{
 const f=await fixture(t,(_req,res)=>{res.writeHead(200,{'content-type':'text/event-stream'});res.end(event({type:'response.created',response:{id:'x'}}));});
 const r=await f.send();assert.equal(r.frames.at(-1).type,'error');assert.equal(r.frames.some(x=>x.type==='done'),false);
});

test('native failure event is redacted, never done',async t=>{
 const f=await fixture(t,(_req,res)=>{res.writeHead(200,{'content-type':'text/event-stream'});res.end(event({type:'response.failed',response:{error:{message:token}}}));});
 const r=await f.send();assert.equal(r.frames.at(-1).type,'error');assert.ok(!r.text.includes(token));assert.equal(r.frames.some(x=>x.type==='chunk'),false);
});

test('redirects never forward credentials',async t=>{
 const f=await fixture(t,(_req,res)=>{res.writeHead(307,{location:'/stolen'});res.end();});const r=await f.send();assert.equal(r.frames.at(-1).status,307);assert.equal(f.requests.length,1);
});

test('request and event limits are enforced',async t=>{
 const f=await fixture(t,(_req,res)=>{res.writeHead(200,{'content-type':'text/event-stream'});res.end(event({type:'unknown',data:'x'.repeat(2048)}));},{limits:{requestBytes:4096,eventBytes:512}});
 assert.equal((await f.send({body:{input:'x'.repeat(5000)}})).status,413);assert.equal(f.requests.length,0);
 const r=await f.send();assert.equal(r.frames.at(-1).code,'response_limit');assert.equal(r.frames.some(x=>x.type==='done'),false);
});

test('client disconnect aborts actual upstream stream',async t=>{
 let disconnected;const closed=new Promise(r=>disconnected=r);
 const f=await fixture(t,(_req,res)=>{res.on('close',disconnected);res.writeHead(200,{'content-type':'text/event-stream'});res.write(event({type:'response.created',response:{id:'x'}}));});
 const controller=new AbortController();const res=await fetch(f.url+'/v1/execute',{method:'POST',headers:{authorization:`Bearer ${secret}`,'content-type':'application/json'},body:JSON.stringify(f.request),signal:controller.signal});
 await res.body.getReader().read();controller.abort();await Promise.race([closed,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error('upstream not cancelled')),3000);timer.unref();})]);
 assert.equal(f.requests.length,1);
});

test('CRLF and split UTF-8 bytes remain identical while Pi parses',async t=>{
 const wire=Buffer.from(native.replace(/\n/g,'\r\n'));
 const f=await fixture(t,(_req,res)=>{
  res.writeHead(200,{'content-type':'text/event-stream'});
  let i=0;const pump=()=>{if(res.destroyed)return;if(i===wire.length){res.end();return;}res.write(wire.subarray(i,i+1));i++;setImmediate(pump);};pump();
 });
 const r=await f.send();assert.equal(r.frames.at(-1).type,'done');
 assert.deepEqual(Buffer.concat(r.frames.filter(x=>x.type==='chunk').map(x=>Buffer.from(x.data,'base64'))),wire);
});

test('total body and unary NDJSON frame limits fail closed',async t=>{
 const f=await fixture(t,(_req,res)=>{res.writeHead(200,{'content-type':'text/event-stream'});res.end(native);},{limits:{responseBytes:512}});
 assert.equal((await f.send()).frames.at(-1).code,'response_limit');
 const g=await fixture(t,undefined,{limits:{frameBytes:512}});
 const r=await g.send({stream:false});assert.equal(r.frames.at(-1).code,'response_limit');assert.equal(r.frames.some(x=>x.type==='result'||x.type==='done'),false);
});

test('deadline cancels hung upstream and no retry occurs',async t=>{
 let closed;const cancelled=new Promise(r=>closed=r);
 const f=await fixture(t,(_req,res)=>{res.on('close',closed);res.writeHead(200,{'content-type':'text/event-stream'});res.write(': heartbeat\n\n');},{limits:{timeoutMs:150}});
 const r=await f.send();assert.equal(r.frames.at(-1).code,'timeout');assert.equal(r.frames.at(-1).dispatch_state,'maybe_sent');
 await cancelled;assert.equal(f.requests.length,1);
});

test('malformed events and incomplete terminals are sanitized failures',async t=>{
 for(const wire of ['data: not-json-'+token+'\n\n',event({type:'response.incomplete',response:{status:'incomplete',error:token}}),event({type:'response.completed',response:{status:'failed',error:token}})]) {
  const f=await fixture(t,(_req,res)=>{res.writeHead(200,{'content-type':'text/event-stream'});res.end(wire);});
  const r=await f.send();assert.equal(r.frames.at(-1).type,'error');assert.equal(r.frames.some(x=>x.type==='chunk'||x.type==='done'),false);assert.ok(!r.text.includes(token));
 }
});

test('authentication, health and loopback-only bind are real HTTP boundaries',async t=>{
 assert.throws(()=>createDriver({token:secret}).listen(0,'0.0.0.0'),/loopback/);
 assert.throws(()=>createDriver({token:'short'}),/token/);
 const f=await fixture(t);assert.equal((await fetch(f.url+'/health')).status,401);
 const r=await fetch(f.url+'/health',{headers:{authorization:`Bearer ${secret}`}});assert.deepEqual(await r.json(),{status:'ok',driver:'pi',version:1});
 assert.equal(f.requests.length,0);
});

test('concurrency cap rejects before a second upstream dispatch',async t=>{
 let started;const ready=new Promise(r=>started=r);
 const f=await fixture(t,(_req,res)=>{res.writeHead(200,{'content-type':'text/event-stream'});res.write(': waiting\n\n');started();},{limits:{concurrent:1}});
 const controller=new AbortController();const first=await fetch(f.url+'/v1/execute',{method:'POST',headers:{authorization:`Bearer ${secret}`,'content-type':'application/json'},body:JSON.stringify(f.request),signal:controller.signal});
 await ready;const second=await f.send();assert.equal(second.status,503);assert.equal(second.frames[0].dispatch_state,'not_sent');assert.equal(f.requests.length,1);
 controller.abort();await first.body.cancel().catch(()=>{});
});

test('native tools, images and reasoning replay are not converted by Pi',async t=>{
 const f=await fixture(t);
 const body={...f.request.body,input:[{role:'user',content:[{type:'input_image',image_url:'data:image/png;base64,c3ludGhldGlj'},{type:'input_text',text:'inspect'}]},...terminal.output,{type:'function_call_output',call_id:'c1',output:'native'}],tools:[{type:'function',name:'one',parameters:{type:'object',properties:{},additionalProperties:false},strict:true}],include:['reasoning.encrypted_content'],text:{format:{type:'json_object'}},metadata:{native:'untouched'}};
 const r=await f.send({body,proxy_url:'direct'});assert.equal(r.frames.at(-1).type,'done');assert.deepEqual(f.requests[0].body,{...body,stream:true});
 assert.equal(f.requests[0].headers['session-id'],'session-fixture');assert.equal(f.requests[0].headers['openai-beta'],'responses=experimental');
});

test('trusted root normalization and custom Pi path convention',async t=>{
 assert.equal(normalizeBaseUrl('https://chatgpt.com'),'https://chatgpt.com/backend-api');
 assert.equal(normalizeBaseUrl('https://chatgpt.com/'),'https://chatgpt.com/backend-api');
 assert.equal(normalizeBaseUrl('https://example.invalid/base/'),'https://example.invalid/base');
 const f=await fixture(t);
 // The origin-only allowlist must not authorize other paths on that origin.
 assert.equal((await f.send({base_url:f.base+'/other'})).status,403);
 assert.equal(f.requests.length,0);
});

test('unread downstream applies backpressure and timeout cancels upstream',async t=>{
 let closed, sent=0;const cancelled=new Promise(r=>closed=r);
 const wire=event({type:'future.native',data:'x'.repeat(32768)});
 const target=4096;
 const f=await fixture(t,(_req,res)=>{
  res.on('close',closed);res.writeHead(200,{'content-type':'text/event-stream'});
  const pump=()=>{while(!res.destroyed && sent<target){sent++;if(!res.write(wire)){res.once('drain',pump);return;}}if(sent===target)res.end(native);};pump();
 },{limits:{timeoutMs:500,responseBytes:256*1024*1024}});
 const req=http.request(f.url+'/v1/execute',{method:'POST',headers:{authorization:`Bearer ${secret}`,'content-type':'application/json'}});
 req.on('error',()=>{});req.end(JSON.stringify(f.request));
 const [response]=await once(req,'response');response.pause();
 t.after(()=>{response.destroy();req.destroy();});
 await Promise.race([cancelled,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error('timeout failed to close upstream')),3000);timer.unref();})]);
 assert.ok(sent<target,`upstream incorrectly drained all ${sent} events despite paused consumer`);
 assert.equal(f.requests.length,1);
});

test('slow request-body admission times out without dispatch',async t=>{
 const f=await fixture(t,undefined,{limits:{timeoutMs:100}});
 const req=http.request(f.url+'/v1/execute',{method:'POST',headers:{authorization:`Bearer ${secret}`,'content-type':'application/json'}});
 req.on('error',()=>{});req.write('{');
 const [res]=await once(req,'response');const chunks=[];for await(const chunk of res)chunks.push(chunk);
 assert.equal(res.statusCode,408);const frame=JSON.parse(Buffer.concat(chunks));assert.equal(frame.dispatch_state,'not_sent');assert.equal(frame.code,'timeout');assert.equal(f.requests.length,0);req.destroy();
});

test('quota metadata survives real Pi for unary and stream without private text', async t => {
 const safe={'x-codex-primary-used-percent':'12.5','x-codex-primary-window-minutes':'300','x-codex-primary-reset-after-seconds':'60','x-codex-secondary-reset-at':'2000000000','x-codex-allowed':'true','x-codex-limit-reached':'false','x-codex-active-limit':'premium','x-codex-bengalfox-primary-used-percent':'25','x-codex-bengalfox-primary-window-minutes':'10080'};
 const f=await fixture(t,(_req,res)=>{res.writeHead(200,{'content-type':'text/event-stream',...safe,'x-codex-private':'SECRET','x-request-id':'SECRET','set-cookie':'SECRET','x-codex-limit-name':'SECRET','x-codex-secondary-used-percent':'101','retry-after':'7'});res.end(native);});
 for(const stream of [false,true]) {const r=await f.send({stream});assert.deepEqual(r.frames[0].headers,{'content-type':'text/event-stream',...safe,'retry-after':'7'});assert.equal(r.frames.at(-1).type,'done');assert.ok(!r.text.includes('SECRET'));}
 assert.equal(f.requests.length,2);
});
test('upstream retry delay is bounded numeric metadata, never a raw error body',async t=>{
 for(const value of ['17','0','-1','1.5','NaN','31622401','SECRET']) {
  const f=await fixture(t,(_req,res)=>{res.writeHead(429,{'retry-after':value});res.end('SECRET');});
  const r=await f.send();assert.equal(r.frames[0].retry_after_seconds,/^(17|0)$/.test(value)?Number(value):undefined);assert.equal(r.frames[0].dispatch_state,'maybe_sent');assert.equal(f.requests.length,1);assert.ok(!r.text.includes('SECRET'));
 }
 const f=await fixture(t,(_req,res)=>{res.writeHead(503,{'retry-after':new Date(Date.now()+60000).toUTCString()});res.end('SECRET');});
 const r=await f.send();assert.ok(r.frames[0].retry_after_seconds>=58 && r.frames[0].retry_after_seconds<=60);assert.equal(f.requests.length,1);
});
