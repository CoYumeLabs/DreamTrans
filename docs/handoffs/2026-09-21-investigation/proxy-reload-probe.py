#!/usr/bin/env python3
"""Isolated proxy-only experiment; neither production nor repository files are changed."""
import concurrent.futures
import http.client
import json
import os
from pathlib import Path
import socket
import statistics
import subprocess
import threading
import time
import uuid
import websocket

ROOT = Path('/tmp/dt-proxy-incident-audit')
PREFIX = 'dt-proxy-audit-' + uuid.uuid4().hex[:10]
NGINX = 'nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10'
CONTAINERS = []
NETWORKS = [PREFIX + '-entry', PREFIX + '-database']
stop = threading.Event()
phase = 'startup'
requests = []
ws_samples = []
errors = []
reload_idle_closures = []
proxy_target = 0

def docker(*args):
    return subprocess.check_output(['docker', *args], text=True, stderr=subprocess.STDOUT).strip()

def port(name):
    value = json.loads(docker('inspect', name))[0]
    return int(value['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort'])

NODE = r'''const http=require('http'),crypto=require('crypto');
const generation=process.env.GENERATION;
const server=http.createServer((req,res)=>{res.setHeader('Content-Type','application/json');res.end(JSON.stringify({generation}));});
server.on('upgrade',(req,socket)=>{
const accept=crypto.createHash('sha1').update(req.headers['sec-websocket-key']+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest('base64');
socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: '+accept+'\r\n\r\n');
const timer=setInterval(()=>{const data=Buffer.from(JSON.stringify({generation,ts:Date.now()}));socket.write(Buffer.concat([Buffer.from([129,data.length]),data]));},50);
socket.on('error',()=>clearInterval(timer));socket.on('close',()=>clearInterval(timer));
socket.on('data',data=>{if((data[0]&15)===8){clearInterval(timer);socket.end(Buffer.from([136,0]));}});
});server.listen(8080,'0.0.0.0');'''

source = Path('/root/DreamTrans/backend/internal/ops/release.go').read_text()
CONFIG = source.split('config := `', 1)[1].split('`', 1)[0]
config_dir = ROOT / PREFIX
config_dir.mkdir()
(config_dir / 'server.js').write_text(NODE)

def start_backend(color, generation):
    name = PREFIX + '-' + color
    CONTAINERS.append(name)
    docker('run','-d','--name',name,'--network',NETWORKS[1],'-p','127.0.0.1::8080',
           '--mount',f'type=bind,src={config_dir},dst=/fixture,readonly','-e',f'GENERATION={generation}',
           'node:24.18.0-alpine3.23','node','/fixture/server.js')
    docker('network','connect',NETWORKS[0],name)
    target = port(name)
    for _ in range(100):
        try:
            conn = http.client.HTTPConnection('127.0.0.1', target, timeout=1)
            conn.request('GET','/healthz')
            response=conn.getresponse(); response.read(); conn.close()
            if response.status == 200: return target
        except OSError: time.sleep(.05)
    raise RuntimeError('fixture did not become ready')

def write_route(color):
    data = CONFIG.replace('COLOR',color).replace('BACKEND',PREFIX+'-'+color)
    candidate=config_dir/'nginx.next'
    candidate.write_text(data)
    candidate.replace(config_dir/'nginx.conf')

def reload(color):
    write_route(color)
    docker('exec',PREFIX+'-proxy','nginx','-t','-c','/release/nginx.conf')
    docker('exec',PREFIX+'-proxy','nginx','-s','reload','-c','/release/nginx.conf')
    # The release controller also waits until a fresh connection sees /_release.
    for _ in range(20):
        conn=http.client.HTTPConnection('127.0.0.1',proxy_target,timeout=3)
        conn.request('GET','/_release'); res=conn.getresponse(); body=res.read(); conn.close()
        if body.decode()==color: return
        time.sleep(.5)
    raise RuntimeError('proxy route acknowledgement failed')

def worker(target, label, reuse):
    conn=None
    i=0
    while not stop.is_set():
        try:
            if conn is None: conn=http.client.HTTPConnection('127.0.0.1',target,timeout=5)
            path='/api/system/settings' if i%2 else '/healthz'
            started=time.perf_counter(); sample_phase=phase
            conn.request('GET',path)
            result=conn.getresponse(); first=time.perf_counter(); body=result.read()
            requests.append({'phase':sample_phase,'path':path,'label':label,'status':result.status,'ttfb_ms':(first-started)*1000,'total_ms':(time.perf_counter()-started)*1000,'generation':json.loads(body).get('generation')})
            if not reuse: conn.close(); conn=None
        except Exception as exc:
            sample={'phase':phase,'kind':'http','label':label,'error':str(exc)}
            if isinstance(exc,http.client.RemoteDisconnected) and reuse:
                reload_idle_closures.append(sample)
            else: errors.append(sample)
            if conn: conn.close()
            conn=None
        i+=1
        stop.wait(.03)
    if conn: conn.close()

def websocket_probe(target, expected):
    ws=websocket.create_connection(f'ws://127.0.0.1:{target}/ws', timeout=3)
    done=threading.Event()
    def loop():
        while not done.is_set():
            try:
                raw=ws.recv()
                if not raw: break
                data=json.loads(raw)
                assert data['generation']==expected, data
                ws_samples.append({'phase':phase,'generation':expected,'latency_ms':time.time()*1000-data['ts']})
            except Exception as exc:
                if not done.is_set(): errors.append({'phase':phase,'kind':'ws','error':str(exc)})
                break
    thread=threading.Thread(target=loop); thread.start()
    def close():
        done.set(); ws.close(); thread.join()
    return close

def summarize(values):
    values=sorted(values)
    return {'n':len(values),'p50':round(statistics.median(values),3),'p95':round(values[min(len(values)-1,int(.95*len(values)))],3),'max':round(max(values),3)}

closers=[]
threads=[]
try:
    for network in NETWORKS: docker('network','create',network)
    direct_blue=start_backend('blue','blue1')
    direct_green=start_backend('green','green1')
    write_route('blue')
    proxy=PREFIX+'-proxy'; CONTAINERS.append(proxy)
    docker('run','-d','--name',proxy,'--network',NETWORKS[0],'-p','127.0.0.1::8080',
           '--mount',f'type=bind,src={config_dir},dst=/release,readonly',NGINX,
           'nginx','-g','daemon off;','-c','/release/nginx.conf')
    docker('network','connect','--alias','dreamtrans',NETWORKS[1],proxy)
    target=port(proxy)
    proxy_target=target
    time.sleep(.5)
    for i in range(8):
        thread=threading.Thread(target=worker,args=(target,'proxy',i%2==0)); thread.start(); threads.append(thread)
    for i in range(2):
        thread=threading.Thread(target=worker,args=(direct_green,'direct-green',True)); thread.start(); threads.append(thread)
    phase='baseline-blue'
    blue_ws=websocket_probe(target,'blue1'); closers.append(blue_ws)
    print('baseline-blue',flush=True); time.sleep(8)
    phase='cutover-green-with-blue-websocket'
    reload('green'); green_ws=websocket_probe(target,'green1'); closers.append(green_ws)
    print(phase,flush=True); time.sleep(12)
    blue_ws(); closers.remove(blue_ws)
    phase='recreate-inactive-blue'
    docker('rm','-f',PREFIX+'-blue')
    start_backend('blue','blue2')
    phase='cutover-recreated-blue-with-green-websocket'
    reload('blue'); blue_ws2=websocket_probe(target,'blue2'); closers.append(blue_ws2)
    print(phase,flush=True); time.sleep(12)
    phase='repeat-reload-same-blue'
    for _ in range(5): reload('blue'); time.sleep(1)
    time.sleep(6)
    stop.set()
    for thread in threads: thread.join()
    for close in closers: close()
    closers=[]
    output={'prefix':PREFIX,'description':'Exact release.go proxy config + production pinned nginx image; synthetic HTTP/WebSocket backends; no Cloudflare, application or database','errors':errors,'reload_idle_connection_closures':reload_idle_closures,'http':{},'websocket':{}}
    for key in sorted({(r['phase'],r['label']) for r in requests}):
        output['http']['/'.join(key)]=summarize([r['ttfb_ms'] for r in requests if (r['phase'],r['label'])==key])
    for key in sorted({(r['phase'],r['generation']) for r in ws_samples}):
        output['websocket']['/'.join(key)]=summarize([r['latency_ms'] for r in ws_samples if (r['phase'],r['generation'])==key])
    output['http_count']=len(requests); output['ws_count']=len(ws_samples)
    (ROOT/'proxy-reload-results.json').write_text(json.dumps(output,indent=2))
    (ROOT/'proxy-reload-raw.json').write_text(json.dumps({'http':requests,'ws':ws_samples,'errors':errors}))
    (ROOT/'proxy-error.log').write_text(docker('logs',proxy))
    print(json.dumps(output,indent=2),flush=True)
finally:
    stop.set()
    for close in closers:
        try: close()
        except Exception: pass
    for thread in threads: thread.join(timeout=6)
    for name in reversed(list(dict.fromkeys(CONTAINERS))):
        subprocess.run(['docker','rm','-f',name],capture_output=True)
    for network in NETWORKS:
        subprocess.run(['docker','network','rm',network],capture_output=True)
