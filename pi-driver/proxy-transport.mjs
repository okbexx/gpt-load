import http from 'node:http';
import https from 'node:https';
import { checkServerIdentity, connect as connectTLS } from 'node:tls';
import net from 'node:net';
import { once } from 'node:events';
import { SocksProxyAgent } from 'socks-proxy-agent';
import { createRequire } from 'node:module';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';

const nativeFetch = globalThis.fetch;
const requirePi = createRequire(import.meta.resolve('@earendil-works/pi-ai'));
const { HttpProxyAgent } = requirePi('http-proxy-agent');
const { HttpsProxyAgent } = requirePi('https-proxy-agent');
const { SocksClient } = createRequire(import.meta.resolve('socks-proxy-agent'))('socks');
const failure = () => new TypeError('Transport request failed');
const aborted = () => new DOMException('Transport request aborted', 'AbortError');

/** null means explicit direct; absent/empty/malformed proxies fail closed. */
export function validateProxyURL(value) {
  if (value === null) return null;
  try {
    if (typeof value !== 'string' || value.length > 4096 || !/^(?:https?|socks5):\/\/[^/?#]+\/?$/.test(value) || /[\s\\\x00-\x1f\x7f]/.test(value)) throw 0;
    const url = new URL(value);
    if (!url.hostname || !['','/'].includes(url.pathname) || url.search || url.hash || value.includes('?') || value.includes('#') || url.port === '0') throw 0;
    for (const part of [url.username,url.password]) {
      const decoded=decodeURIComponent(part);
      if (decoded.length>1024 || /[\x00-\x1f\x7f]/.test(decoded)) throw 0;
      if (url.protocol==='socks5:' && Buffer.byteLength(decoded)>255) throw 0;
    }
    return url;
  } catch { throw new TypeError('Invalid proxy configuration'); }
}

/**
 * Single-attempt native fetch with a private Node HTTP dispatcher. The third
 * argument is mandatory: null for direct, or an admitted HTTP(S)/SOCKS5 proxy URL.
 * Inject `(input, init) => transport(input, init, selectedProxy)` into Pi.
 * ca/proxyCA add explicit trust; no insecure TLS option is exposed.
 */
export function createProxyFetch({ ca, proxyCA = ca, connectTimeoutMs = 30_000 } = {}) {
  if (!Number.isSafeInteger(connectTimeoutMs) || connectTimeoutMs < 1 || connectTimeoutMs > 300_000) throw new TypeError('Invalid transport timeout');
  return async function proxyFetch(input, init = {}, proxyURL) {
    const proxy = validateProxyURL(proxyURL);
    let request;
    try {
      request = new Request(input, { ...init, redirect:'manual' });
      const target = new URL(request.url);
      if (!['http:','https:'].includes(target.protocol) || target.username || target.password) throw 0;
      // Proxy credentials can only originate from the explicitly selected proxy.
      request.headers.delete('proxy-authorization');
      request.headers.delete('proxy-connection');
      // Host must be derived from the URL, including for absolute-form forwarding.
      request.headers.delete('host');
    } catch { throw new TypeError('Invalid transport request'); }
    if (request.signal.aborted) throw aborted();
    const cancellation = new AbortController();
    let dispatcher;
    const onAbort = () => { cancellation.abort(aborted()); dispatcher?.abort(); };
    request.signal.addEventListener('abort', onAbort, { once:true });
    const detach = () => request.signal.removeEventListener('abort', onAbort);
    dispatcher = new SingleAttemptDispatcher({ proxy, ca, proxyCA, connectTimeoutMs, detach });
    try {
      const response = await nativeFetch(request, { dispatcher, redirect:'manual', signal:cancellation.signal });
      if (!response.body) return response;
      const reader = response.body.getReader();
      const body = new ReadableStream({
        async pull(controller) {
          try {
            const chunk = await reader.read();
            if (chunk.done) controller.close(); else controller.enqueue(chunk.value);
          } catch (error) { dispatcher.abort(); controller.error(error); }
        },
        async cancel(reason) {
          dispatcher.abort();
          await reader.cancel(reason).catch(() => {});
        },
      });
      return new Response(body, {status: response.status, statusText: response.statusText, headers: response.headers});
    } catch {
      detach();
      if (request.signal.aborted) throw aborted();
      throw failure();
    }
  };
}

// Native fetch owns Response construction, decompression, SSE byte streams and
// WHATWG backpressure. This narrow dispatcher never consults global agents or
// environment proxies, and deliberately has no pooling, redirects or retries.
// socks-proxy-agent 8 creates its handshake socket before exposing it to the
// agent; agent.destroy() cannot cancel it, and socket_options.signal is passed
// to Socket.connect(), not the Socket constructor (Node 22 ignores it there).
// Keep its URL/auth parsing and Agent machinery, but negotiate using the same
// dependency's existing_socket API so cancellation owns the socket from birth.
class CancellableSocksProxyAgent extends SocksProxyAgent {
  constructor(url, signal, timeout) {
    super(url, {keepAlive:false});
    this.signal=signal;
    this.handshakeTimeout=timeout;
    this.activeSocket=null;
    this.rawSocket=null;
  }
  destroy() {
    for (const socket of [this.activeSocket, this.rawSocket]) {
      if (socket && !socket.destroyed) {
        socket.end?.();
        socket.destroy();
      }
    }
    this.activeSocket=null;
    this.rawSocket=null;
    return super.destroy();
  }
  async connect(_req, opts) {
    const socket=new net.Socket({signal:this.signal});
    let tlsSocket;
    try {
      const connected=once(socket,'connect');
      socket.connect({host:this.proxy.host.replace(/^\[|\]$/g,''),port:this.proxy.port});
      await connected;
      if(this.signal.aborted)throw aborted();
      const host=opts.host.replace(/^\[|\]$/g,'');
      const tunnel=await SocksClient.createConnection({
        proxy:this.proxy, destination:{host,port:Number(opts.port)},
        command:'connect',existing_socket:socket,timeout:this.handshakeTimeout,
      });
      const tunnelSocket=tunnel?.socket ?? socket;
      if(this.signal.aborted)throw aborted();
      this.activeSocket=tunnelSocket;
      this.rawSocket=socket;
      if(!opts.secureEndpoint)return tunnelSocket;
      // Never resolve the destination locally. SNI/identity still name the
      // origin, not the SOCKS endpoint or an address chosen by its resolver.
      const {host:_host,path:_path,port:_port,...tlsOptions}=opts;
      tlsSocket=connectTLS({...tlsOptions,socket:tunnelSocket,signal:this.signal,
        servername:opts.servername ?? (net.isIP(host)?undefined:host)});
      tlsSocket.once('error',()=>tunnelSocket.destroy());
      // TLS takes ownership of the raw handle. Explicitly destroy the wrapper
      // as well: aborting only the original Socket may no longer close it.
      const cancel=()=>{tlsSocket.destroy();tunnelSocket.destroy();socket.destroy();};
      this.signal.addEventListener('abort',cancel,{once:true});
      tlsSocket.once('close',()=>this.signal.removeEventListener('abort',cancel));
      if(this.signal.aborted)cancel();
      this.activeSocket=tlsSocket;
      return tlsSocket;
    } catch(error) { tlsSocket?.destroy();socket.destroy();throw error; }
  }
}

class SingleAttemptDispatcher {
  constructor(options) { this.options=options; this.used=false; }
  abort() { this.abortFn?.(); }
  dispatch(options, handler) {
    if (this.used) { handler.onError(failure()); return false; }
    this.used=true;
    const { proxy, ca, proxyCA, connectTimeoutMs }=this.options;
    const target=new URL(options.path, options.origin);
    const secure=target.protocol==='https:';
    const connection=new AbortController();
    let agent, req, res, upload, socket, timer, finished=false;
    const cleanup=()=>{ clearTimeout(timer); this.options.detach(); upload?.destroy(); agent?.destroy(); connection.abort(); res?.socket?.destroy(); socket?.destroy(); req?.socket?.destroy(); req?.destroy(); };
    this.abortFn=()=>{ if(!finished){ finished=true; res?.destroy(); req?.destroy(); cleanup(); } };
    const fail=()=>{
      if(finished)return;
      finished=true;
      upload?.destroy(); res?.destroy(); req?.destroy(); cleanup();
      handler.onError(failure());
    };
    try {
      if(proxy?.protocol==='socks5:') {
        // Go x/net/proxy.SOCKS5 sends FQDNs remotely; npm's socks5 spelling
        // requests local DNS. Normalize only the private agent URL to socks5h.
        const url=new URL(proxy);url.protocol='socks5h:';
        agent=new CancellableSocksProxyAgent(url,connection.signal,connectTimeoutMs);
      } else if(proxy) {
        // Keep userinfo out of dependency debug logging of proxy.href.
        const url=new URL(proxy);
        const auth=url.username||url.password ? `Basic ${Buffer.from(`${decodeURIComponent(url.username)}:${decodeURIComponent(url.password)}`).toString('base64')}` : undefined;
        url.username='';url.password='';
        agent=new (secure?HttpsProxyAgent:HttpProxyAgent)(url, {
          keepAlive:false, ca:proxyCA, rejectUnauthorized:true, signal:connection.signal,
          headers:auth?{'Proxy-Authorization':auth}:{},
        });
      } else {
        agent=new (secure?https.Agent:http.Agent)({keepAlive:false,ca,rejectUnauthorized:true});
      }
      const headers={};
      if (Array.isArray(options.headers)) {
        for(let i=0;i<options.headers.length;i+=2) headers[String(options.headers[i]).toLowerCase()]=options.headers[i+1];
      } else {
        for (const [key,value] of Object.entries(options.headers ?? {})) headers[key.toLowerCase()]=value;
      }
      delete headers['proxy-authorization'];delete headers['proxy-connection'];delete headers.host;
      req=(secure?https:http).request(target, {
        method:options.method,headers,agent,ca,rejectUnauthorized:true,
        // CONNECT agents omit host when upgrading an existing socket. For IP
        // targets without SNI, Node would otherwise verify against localhost.
        checkServerIdentity:(_hostname,cert)=>checkServerIdentity(target.hostname.replace(/^\[|\]$/g,''),cert),
      });
      req.once('close',()=>{if(!finished)agent?.destroy();});
      req.once('socket',s=>{ socket=s; if(finished)s.destroy(); });
      req.on('error',fail);
      req.on('response',response=>{
        if(finished){response.destroy();return;}
        res=response;clearTimeout(timer);
        res.on('error',fail);res.on('aborted',fail);
        // Native Response.body.cancel() closes IncomingMessage without
        // necessarily emitting `aborted`; close is the only notification and
        // must tear down the SOCKS/TLS tunnel before the caller returns.
        res.on('close',()=>{if(!finished){finished=true;cleanup();}});
        res.on('data',chunk=>{if(!handler.onData(chunk))res.pause();});
        res.on('end',()=>{
          if(finished)return;finished=true;
          handler.onComplete(res.rawTrailers.map(x=>Buffer.from(x)));cleanup();
        });
        if(!handler.onHeaders(res.statusCode,res.rawHeaders.map(x=>Buffer.from(x)),()=>res.resume(),res.statusMessage))res.pause();
      });
      // Do not buffer credential-bearing request bytes in proxy-agent's debug
      // outputData path. Waiting for socket assignment also bounds upload work.
      req.once('socket',()=>{
        if(finished)return;
        if(options.body) {
          upload=Readable.from(options.body);
          pipeline(upload,req).catch(fail);
        } else req.end();
      });
      timer=setTimeout(fail,connectTimeoutMs);timer.unref();
      handler.onConnect(fail);
    } catch { fail(); }
    return true;
  }
}
