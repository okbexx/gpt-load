import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import { once } from 'node:events';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync } from 'node:child_process';
import { gzipSync } from 'node:zlib';
import { createProxyFetch, validateProxyURL } from '../proxy-transport.mjs';

async function listen(t, server) {
  const sockets = new Set();
  server.on('connection', s => { sockets.add(s); s.on('close', () => sockets.delete(s)); });
  server.listen(0, '127.0.0.1'); await once(server, 'listening');
  t.after(() => { for (const s of sockets) s.destroy(); server.close(); });
  return server.address().port;
}
async function fixture(t, secure = false) {
  let tls;
  if (secure) {
    const dir = mkdtempSync(join(tmpdir(), 'pi-transport-'));
    t.after(() => rmSync(dir, { recursive: true, force: true }));
    execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', join(dir,'key'), '-out',join(dir,'cert'), '-days','1','-subj','/CN=localhost','-addext','subjectAltName=DNS:localhost,IP:127.0.0.1'], {stdio:'ignore'});
    tls = { key:readFileSync(join(dir,'key')),cert:readFileSync(join(dir,'cert')) };
  }
  const seen = [];
  const handler = async (req, res) => {
    const chunks=[]; for await (const c of req) chunks.push(c);
    seen.push({headers:req.headers,body:Buffer.concat(chunks),url:req.url});
    if(req.url==='/stream') { res.writeHead(200, {'content-type':'text/event-stream'}); res.write('data: first\n\n'); return; }
    if(req.url==='/redirect') { res.writeHead(302,{location:'/destination'}); res.end(); return; }
    if(req.url==='/compressed') { res.writeHead(200,{'content-encoding':'gzip'}); res.end(gzipSync('decoded')); return; }
    res.end('ok');
  };
  const origin=secure?https.createServer(tls,handler):http.createServer(handler);
  const originSockets=new Set();
  origin.on('connection',socket=>{originSockets.add(socket);socket.on('close',()=>originSockets.delete(socket));});
  const port=await listen(t,origin);
  const proxySeen=[];
  const forward=(req,res)=>{
    proxySeen.push(req.headers);
    const target=new URL(req.url); const headers={...req.headers}; delete headers['proxy-authorization']; delete headers['proxy-connection'];
    const upstream=http.request(target,{method:req.method,headers},r=>{res.writeHead(r.statusCode,r.headers);r.pipe(res);});
    res.on('close',()=>upstream.destroy());
    upstream.on('error',()=>res.destroy());req.pipe(upstream);
  };
  const proxy=secure?https.createServer(tls,forward):http.createServer(forward);
  proxy.on('connect',(req,client,head)=>{
    proxySeen.push(req.headers);
    const upstream=net.connect(port,'127.0.0.1',()=>{client.write('HTTP/1.1 200 Connection Established\r\n\r\n'); if(head.length)upstream.write(head);client.pipe(upstream);upstream.pipe(client);});
    client.on('error',()=>upstream.destroy()); client.on('close',()=>upstream.destroy()); upstream.on('error',()=>client.destroy());
  });
  const proxyPort=await listen(t,proxy);
  return {origin,originSockets,seen,proxySeen,ca:tls?.cert,url:`${secure?'https':'http'}://127.0.0.1:${port}`,proxyURL:`${secure?'https':'http'}://user:secret@127.0.0.1:${proxyPort}`};
}

// Byte-level loopback SOCKS5 server: never resolves or contacts external hosts.
async function socksFixture(t, originPort, { auth = true, stall, deny = false } = {}) {
  const handshakes=[]; const destinations=[]; const sockets=new Set();
  let reachedResolve; const reached=new Promise(r=>reachedResolve=r);
  const server=net.createServer(client=>{
    sockets.add(client);client.on('close',()=>sockets.delete(client));
    client.on('error',()=>{});client.on('end',()=>client.destroy());
    let buffer=Buffer.alloc(0), stage='greeting';
    const consume=n=>{const b=buffer.subarray(0,n);buffer=buffer.subarray(n);return b;};
    const receive=chunk=>{
      buffer=Buffer.concat([buffer,chunk]);
      while(true) {
        if(stage==='greeting') {
          if(buffer.length<2 || buffer.length<2+buffer[1])return;
          const greeting=consume(2+buffer[1]);
          assert.equal(greeting[0],5);assert.ok(greeting.subarray(2).includes(auth?2:0));
          if(stall==='greeting'){stage='stall';reachedResolve();return;}
          client.write(Buffer.from([5,auth?2:0]));stage=auth?'auth':'connect';
        } else if(stage==='auth') {
          if(buffer.length<2 || buffer.length<3+buffer[1])return;
          const end=3+buffer[1]+buffer[2+buffer[1]];if(buffer.length<end)return;
          const bytes=consume(end);assert.equal(bytes[0],1);
          handshakes.push({user:bytes.subarray(2,2+bytes[1]).toString(),password:bytes.subarray(3+bytes[1]).toString()});
          if(stall==='auth'){stage='stall';reachedResolve();return;}
          client.write(Buffer.from([1,deny?1:0]));stage=deny?'stall':'connect';
        } else if(stage==='connect') {
          if(buffer.length<5)return;
          const atyp=buffer[3];const size=atyp===3?buffer[4]:atyp===1?4:16;
          const start=atyp===3?5:4;if(buffer.length<start+size+2)return;
          const bytes=consume(start+size+2);
          assert.deepEqual([...bytes.subarray(0,3)],[5,1,0]);
          destinations.push({atyp,host:atyp===3?bytes.subarray(start,start+size).toString():[...bytes.subarray(start,start+size)].join('.'),port:bytes.readUInt16BE(start+size)});
          if(stall==='connect'){stage='stall';reachedResolve();return;}
          stage='tunnel';client.removeListener('data',receive);
          const upstream=net.connect(originPort,'127.0.0.1',()=>{
            client.write(Buffer.from([5,0,0,1,127,0,0,1,0,0]));
            if(buffer.length)upstream.write(buffer);client.pipe(upstream);upstream.pipe(client);
          });
          client.on('close',()=>upstream.destroy());upstream.on('error',()=>client.destroy());
          upstream.on('close',()=>{
            // A closed pipe destination can leave the client paused with TLS
            // close_notify bytes buffered. Drain them to observe real peer EOF;
            // destroying the client here would hide missing transport shutdown.
            client.unpipe(upstream);client.resume();
          });
          return;
        } else return;
      }
    };
    client.on('data',receive);
  });
  const port=await listen(t,server);
  return {url:`socks5://${auth?'us%40er:p%C3%A4ss%3Aword@':''}127.0.0.1:${port}`,handshakes,destinations,sockets,reached};
}
async function boundedClosed(sockets, label = 'SOCKS') {
  const deadline=Date.now()+2000;
  while(sockets.size && Date.now()<deadline)await new Promise(r=>setTimeout(r,10));
  assert.equal(sockets.size,0,`${label} socket must close within two seconds`);
}
test('SOCKS5 strict decoded byte bounds and unchanged public scheme',()=>{
  assert.equal(validateProxyURL('socks5://user:pass@localhost:1080').protocol,'socks5:');
  for(const auth of ['u'.repeat(256)+':p','u:'+encodeURIComponent('é'.repeat(128)),'u:%00','u:%GG']) {
    assert.throws(()=>validateProxyURL(`socks5://${auth}@localhost:1080`),/Invalid proxy configuration/);
  }
  assert.ok(validateProxyURL(`socks5://${'u'.repeat(255)}:${'p'.repeat(255)}@localhost:1080`));
});
for(const secure of [false,true])test(`SOCKS5 real ${secure?'HTTPS':'HTTP'} remote DNS, auth, byte preservation and stream cancellation`,async t=>{
  const f=await fixture(t,secure);const s=await socksFixture(t,Number(new URL(f.url).port));
  const fetch=createProxyFetch({ca:f.ca});
  // .invalid cannot resolve locally; the fixture alone maps it to loopback.
  const url=f.url.replace('127.0.0.1',secure?'localhost':'only-at-proxy.invalid');
  const body=gzipSync('SOCKS exact compressed bytes');
  const r=await fetch(url,{method:'POST',body,headers:{authorization:'Bearer SOCKS-Origin-Exact','proxy-authorization':'caller-must-not-leak','content-encoding':'gzip'}},s.url);
  assert.equal(await r.text(),'ok');assert.deepEqual(f.seen[0].body,body);
  assert.equal(f.seen[0].headers.authorization,'Bearer SOCKS-Origin-Exact');
  assert.equal(f.seen[0].headers['proxy-authorization'],undefined);
  assert.deepEqual(s.handshakes,[{user:'us@er',password:'päss:word'}]);
  assert.deepEqual(s.destinations,[{atyp:3,host:secure?'localhost':'only-at-proxy.invalid',port:Number(new URL(f.url).port)}]);
  await boundedClosed(s.sockets);
  await boundedClosed(f.originSockets,'origin');
  for(const mode of ['abort','cancel']) {
    const controller=new AbortController();
    const stream=await fetch(url+'/stream',{signal:controller.signal},s.url);
    const reader=stream.body.getReader();assert.match(new TextDecoder().decode((await reader.read()).value),/data: first/);
    assert.equal(f.originSockets.size,1,'stream must have a live origin socket before cancellation');
    if(mode==='cancel')await reader.cancel();else {controller.abort('secret');await assert.rejects(reader.read(),{name:'AbortError'});}
    await Promise.all([boundedClosed(s.sockets),boundedClosed(f.originSockets,'origin')]);
  }
  if(secure) {
    await assert.rejects(createProxyFetch()(url,{},s.url),/Transport request failed/);
    await boundedClosed(s.sockets);
    await assert.rejects(fetch(url.replace('localhost','wrong.invalid'),{},s.url),/Transport request failed/);
    await boundedClosed(s.sockets);
    assert.equal(f.seen.length,3);
  }
});
test('SOCKS5 no-auth IP target, auth denial and no fallback',async t=>{
  const f=await fixture(t);const port=Number(new URL(f.url).port);
  const plain=await socksFixture(t,port,{auth:false});
  assert.equal(await (await createProxyFetch()(f.url,{},plain.url)).text(),'ok');
  assert.equal(plain.destinations[0].atyp,1);assert.equal(plain.handshakes.length,0);
  await boundedClosed(plain.sockets);
  const denied=await socksFixture(t,port,{deny:true});
  await assert.rejects(createProxyFetch()(f.url,{headers:{authorization:'Bearer secret'},method:'POST',body:'secret'},denied.url),e=>e.message==='Transport request failed');
  assert.equal(denied.handshakes.length,1);assert.equal(denied.destinations.length,0);assert.equal(f.seen.length,1);
  await boundedClosed(denied.sockets);
});
for(const stage of ['greeting','auth','connect'])test(`SOCKS5 stalled ${stage}: abort and deadline close pending sockets`,async t=>{
  const f=await fixture(t);
  for(const mode of ['abort','timeout']) {
    const s=await socksFixture(t,Number(new URL(f.url).port),{stall:stage});
    const controller=new AbortController();const fetch=createProxyFetch({connectTimeoutMs:mode==='timeout'?200:30000});
    const rejection=assert.rejects(fetch(f.url,{signal:controller.signal},s.url),mode==='abort'?{name:'AbortError'}:/Transport request failed/);
    await s.reached;if(mode==='abort')controller.abort('secret');await rejection;
    await boundedClosed(s.sockets);
  }
  assert.equal(f.seen.length,0);
});

test('strict bounded proxy admission',()=>{
  for(const value of ['socks4://localhost:1','socks5h://localhost:1','http://localhost/path','http://localhost?x=secret','http://localhost#x',' http://localhost','http://u:%GG@localhost','http://localhost:0','http://localhost\\evil','x'.repeat(4097)]) assert.throws(()=>validateProxyURL(value), /Invalid proxy configuration/);
  assert.equal(validateProxyURL(null),null);
});
for (const secure of [false,true]) test(`real ${secure?'HTTPS CONNECT + TLS':'HTTP forward'}: exact bytes, auth, SSE, cleanup`,async t=>{
  const f=await fixture(t,secure); const fetch=createProxyFetch({ca:f.ca});
  const body=gzipSync('exact compressed body');
  const r=await fetch(f.url,{method:'POST',headers:{authorization:'Bearer exact-Token','content-encoding':'gzip','proxy-authorization':'must-not-leak'},body},f.proxyURL);
  assert.ok(r instanceof Response); assert.equal(await r.text(),'ok');
  assert.deepEqual(f.seen[0].body,body);assert.equal(f.seen[0].headers.authorization,'Bearer exact-Token');assert.equal(f.seen[0].headers['proxy-authorization'],undefined);
  assert.equal(f.proxySeen[0]['proxy-authorization'],'Basic dXNlcjpzZWNyZXQ=');
  if(secure)assert.equal(f.proxySeen[0].authorization,undefined);
  const s=await fetch(f.url+'/stream',{},f.proxyURL);const reader=s.body.getReader();assert.match(new TextDecoder().decode((await reader.read()).value),/data: first/); await reader.cancel();
  const c=await fetch(f.url+'/compressed',{},f.proxyURL);assert.equal(await c.text(),'decoded');
  const red=await fetch(f.url+'/redirect',{},f.proxyURL);assert.equal(red.status,302);await red.body.cancel();assert.equal(f.seen.some(x=>x.url==='/destination'),false);
});
test('cross-scheme proxies and independently verified origin TLS',async t=>{
  const plain=await fixture(t);const secure=await fixture(t,true);
  // A plain HTTP proxy tunnels to the trusted TLS origin.
  const tunnel=http.createServer();
  tunnel.on('connect',(req,client)=>{
    const upstream=net.connect(new URL(secure.url).port,'127.0.0.1',()=>{client.write('HTTP/1.1 200 OK\r\n\r\n');client.pipe(upstream);upstream.pipe(client);});
    client.on('close',()=>upstream.destroy());upstream.on('error',()=>client.destroy());
  });
  const port=await listen(t,tunnel);const proxy=`http://127.0.0.1:${port}`;
  assert.equal(await (await createProxyFetch({ca:secure.ca})(secure.url,{},proxy)).text(),'ok');
  await assert.rejects(createProxyFetch()(secure.url,{},proxy),/Transport request failed/);
  await assert.rejects(createProxyFetch({ca:secure.ca})(secure.url.replace('127.0.0.1','127.0.0.2'),{},proxy),/Transport request failed/);
  assert.equal(secure.seen.length,1);
  // TLS forward proxy can carry a plain HTTP origin.
  assert.equal(await (await createProxyFetch({proxyCA:secure.ca})(plain.url,{},secure.proxyURL)).text(),'ok');
  // Trusting the proxy does not implicitly trust the TLS origin.
  await assert.rejects(createProxyFetch({proxyCA:secure.ca})(secure.url,{},secure.proxyURL),/Transport request failed/);
  assert.equal(secure.seen.length,1);
});

test('pending CONNECT abort/timeout closes proxy socket, no origin dispatch',async t=>{
  const f=await fixture(t,true);
  for(const mode of ['abort','timeout']) {
    const proxy=http.createServer();let connectedResolve,closedResolve;
    const connected=new Promise(r=>connectedResolve=r);const closed=new Promise(r=>closedResolve=r);
    proxy.on('connect',(_req,s)=>{s.on('close',closedResolve);s.on('end',()=>s.destroy());s.resume();connectedResolve();});
    const port=await listen(t,proxy);const controller=new AbortController();
    const fetch=createProxyFetch({ca:f.ca,connectTimeoutMs:mode==='timeout'?100:2000});
    const pending=fetch(f.url,{signal:controller.signal},`http://user:secret@127.0.0.1:${port}`);
    const rejection=assert.rejects(pending,mode==='abort'?{name:'AbortError'}:/Transport request failed/);
    await connected;if(mode==='abort')controller.abort('secret');await rejection;
    await Promise.race([closed,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(new Error('pending proxy socket leaked')),2000);timer.unref();})]);
  }
  assert.equal(f.seen.length,0);
});

test('CONNECT denial does not send origin authorization or body to proxy',async t=>{
  const f=await fixture(t,true);const proxy=http.createServer();const received=[];
  proxy.on('connect',(req,s,head)=>{received.push({headers:req.headers,head});s.end('HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n');});
  const port=await listen(t,proxy);
  await assert.rejects(createProxyFetch({ca:f.ca})(f.url,{method:'POST',body:'origin-secret',headers:{authorization:'Bearer origin-secret'}},`http://127.0.0.1:${port}`),/Transport request failed/);
  assert.equal(received.length,1);assert.equal(received[0].headers.authorization,undefined);assert.equal(received[0].head.length,0);assert.equal(f.seen.length,0);
});

test('concurrent direct/proxy routes cannot share credentials',async t=>{
  const f=await fixture(t);const fetch=createProxyFetch();
  await Promise.all(['one','two',null].map(async user=>{
    const proxy=user?f.proxyURL.replace('user:secret',`${user}:password`):null;
    const r=await fetch(f.url+'/'+(user??'direct'),{headers:{authorization:`Bearer ${user??'direct'}`}},proxy);await r.text();
  }));
  assert.deepEqual(new Set(f.proxySeen.map(h=>h['proxy-authorization'])),new Set(['Basic b25lOnBhc3N3b3Jk','Basic dHdvOnBhc3N3b3Jk']));
  for(const record of f.seen){assert.equal(record.headers.authorization,`Bearer ${record.url.slice(1)}`);assert.equal(record.headers['proxy-authorization'],undefined);}
});

test('unread response is backpressured and cancellation closes direct socket',async t=>{
  let written=0;let closedResolve;const closed=new Promise(r=>closedResolve=r);
  const server=http.createServer((_req,res)=>{
    res.on('close',closedResolve);
    const chunk=Buffer.alloc(64*1024);
    const pump=()=>{while(!res.destroyed&&written<64*1024*1024){written+=chunk.length;if(!res.write(chunk)){res.once('drain',pump);return;}}res.end();};pump();
  });
  const port=await listen(t,server);const r=await createProxyFetch()(`http://127.0.0.1:${port}`,{},null);
  await new Promise(resolve=>setTimeout(resolve,100));
  assert.ok(written<64*1024*1024,'unread body must not be eagerly drained');
  await r.body.cancel();await closed;
});

test('HEAD and empty responses retain native Response semantics',async t=>{
  const server=http.createServer((_req,res)=>{res.writeHead(204,{'x-contract':'empty'});res.end();});const port=await listen(t,server);
  for(const method of ['HEAD','GET']) {const r=await createProxyFetch()(`http://127.0.0.1:${port}`,{method},null);assert.equal(r.status,204);assert.equal(r.body,null);assert.equal(await r.text(),'');}
});

test('TLS rejects untrusted origin/proxy and never falls back',async t=>{
  const f=await fixture(t,true); const fetch=createProxyFetch();
  await assert.rejects(fetch(f.url,{},f.proxyURL),/Transport request failed/);assert.equal(f.seen.length,0);
  await assert.rejects(fetch(f.url,{},null),/Transport request failed/);assert.equal(f.seen.length,0);
});
test('failed proxy cannot dispatch origin; explicit direct ignores environment',async t=>{
  const f=await fixture(t);const fetch=createProxyFetch();const dead=http.createServer();const port=await listen(t,dead);await new Promise(r=>dead.close(r));
  await assert.rejects(fetch(f.url,{},`http://user:secret@127.0.0.1:${port}`),e=>!String(e).includes('secret'));assert.equal(f.seen.length,0);
  const previous=process.env.HTTP_PROXY;process.env.HTTP_PROXY=`http://127.0.0.1:${port}`;t.after(()=>{if(previous===undefined)delete process.env.HTTP_PROXY;else process.env.HTTP_PROXY=previous;});
  // Poison the native fetch global dispatcher; explicit direct must bypass it.
  const key=Symbol.for('undici.globalDispatcher.1');const previousDispatcher=globalThis[key];
  globalThis[key]={dispatch(){throw new Error('global dispatcher used');}};
  t.after(()=>{globalThis[key]=previousDispatcher;});
  assert.equal(await (await fetch(f.url,{},null)).text(),'ok');
});
test('abort before dispatch and after headers; unread body cancellation closes socket',async t=>{
  const f=await fixture(t);const fetch=createProxyFetch();const already=new AbortController();already.abort();
  await assert.rejects(fetch(f.url,{signal:already.signal},f.proxyURL),{name:'AbortError'});assert.equal(f.seen.length,0);
  for(const mode of ['abort','cancel']) {
    const controller=new AbortController();const closed=new Promise(resolve=>f.origin.once('connection',s=>s.once('close',resolve)));
    const r=await fetch(f.url+'/stream',{signal:controller.signal},f.proxyURL);
    if(mode==='abort'){controller.abort('credential-secret');await assert.rejects(r.text(),e=>e.name==='AbortError'&&!String(e).includes('credential-secret'));}else await r.body.cancel();
    await Promise.race([closed,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(new Error('socket not closed')),2000);timer.unref();})]);
  }
});
