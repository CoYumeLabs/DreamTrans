#!/usr/bin/env python3
"""Full isolated old -> new -> configure-edge deployment with mock supplier audio.
Only disposable Docker resources, synthetic accounts/PCM/credentials are used.
"""
import base64, concurrent.futures, hashlib, hmac, json, os, statistics, subprocess, threading, time, urllib.request, urllib.error, uuid
from pathlib import Path
import websocket

ROOT=Path(__file__).resolve().parent
OLD='ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90'
NEW='ghcr.io/coyumelabs/dreamtrans@sha256:a6fcde016bec5d69d78f4bdf0fcdbb9bf85df3c6755f4c1c6b529bbb3661b64d'
EDGE='ghcr.io/coyumelabs/dreamtrans@sha256:ff89889f36ba5c9f3fde9ee87bd8781d6262c4dede0ad521aeaf44a7e4aab97e'
PROXY='nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10'
PG='pgvector/pgvector:0.8.2-pg16-bookworm'
name='dt-perf-'+uuid.uuid4().hex[:8]; network=name+'-dbnet'; db=name+'-db'; legacy=name+'-legacy'; provider=name+'-provider'
runroot=ROOT/name; runroot.mkdir(); os.chmod(runroot,0o700)
prefix='dreamtrans-'+hashlib.sha256(str(runroot).encode()).hexdigest()[:10]
containers=[db,legacy,provider]+[prefix+'-'+x for x in ['blue','green','proxy']]
volumes=[name+'-pgdata',name+'-appdata']; networks=[network,prefix+'-entry']
uid,tid,sid=[str(uuid.uuid4()) for _ in range(3)]
key='0123456789abcdef0123456789abcdef'
stop=threading.Event(); stage='setup'; records=[]; audio_records=[]; errors=[]; tasks=[]; audio_connections=[]
stable_stages=['old_active','new_http_old_audio_draining','new_active_before_config','new_http_new_audio_draining','new_active_after_config']
cutover_stages=['upgrade_to_new','configure_edge_same_image']
report={'valid':False,'old':OLD,'new':NEW,'fixture':'synthetic 100k usage rows, mock provider PCM acknowledgements, real release CLI/Nginx/PG',
        'boundaries':['Provider only acknowledges synthetic PCM; no actual transcription, subtitle persistence, translation, or supplier latency is measured.',
                      'Short 20–30 second stable phases do not exercise the production 600 second handoff timer or long-running background work.',
                      'No Cloudflare tunnel or real Edge registration; independent per-container 2 CPU limits are not a shared 2 vCPU t3.medium host or its CPU/EBS credits.'],
        'stage_applications':{}}

def run(*args, input_text=None, timeout=180):
    p=subprocess.run(args,input=input_text,text=True,capture_output=True,timeout=timeout)
    if p.returncode: raise RuntimeError(str(args[:3])+': '+p.stderr[-2000:]+p.stdout[-1000:])
    return p.stdout.strip()
def inspect(name): return json.loads(run('docker','inspect',name))[0]
def sql(query): return run('docker','exec','-i',db,'psql','-XAt','-v','ON_ERROR_STOP=1','-U','fixture','-d','fixture',input_text=query)
def state(): return json.loads((runroot/'.bluegreen/state.json').read_text())
def cli(*args):
    value=run(str(ROOT/'dreamtransctl'),'--dir',str(runroot),*args)
    with (ROOT/'lifecycle.log').open('a') as f: f.write(value+'\n')
    return value
def enc(x): return base64.urlsafe_b64encode(json.dumps(x,separators=(',',':')).encode()).decode().rstrip('=')
data=enc({'alg':'HS256','typ':'JWT'})+'.'+enc({'user_id':uid,'tenant_id':tid,'email':'fixture@example.test','role':'super_admin','token_type':'access','iss':'dreamtrans','jti':str(uuid.uuid4()),'sub':uid,'exp':int(time.time())+3600,'iat':int(time.time())-1,'nbf':int(time.time())-1})
token=data+'.'+base64.urlsafe_b64encode(hmac.new(key.encode(),data.encode(),hashlib.sha256).digest()).decode().rstrip('=')
base=''
def request(path):
    start=time.perf_counter(); label=stage; status=0
    try:
        req=urllib.request.Request(base+path,headers={'Authorization':'Bearer '+token})
        with urllib.request.urlopen(req,timeout=25) as r:
            status=r.status;first=(time.perf_counter()-start)*1000;r.read()
    except urllib.error.HTTPError as e:
        status=e.code;first=(time.perf_counter()-start)*1000
    except Exception as e:
        first=(time.perf_counter()-start)*1000;errors.append({'stage':label,'kind':'http','error':type(e).__name__})
    records.append({'stage':label,'path':path.split('?')[0],'status':status,'ttfb_ms':round(first,3),'total_ms':round((time.perf_counter()-start)*1000,3)})
    return status
def load():
    paths=['/healthz','/readyz','/api/system/settings','/api/system/access','/api/prompts/defaults','/api/user/profile','/api/admin/access','/api/user/billing/account','/api/user/billing/session-costs?session_ids='+sid]
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
        while not stop.is_set():
            list(ex.map(request,paths));stop.wait(.1)
def audio(done):
    c=None; receiver=None; failed=threading.Event(); ended=threading.Event(); lock=threading.Lock()
    pending={}; seen=set(); sequence=0; connection_id=uuid.uuid4().hex; duplicates=[]
    def receive():
        try:
            while True:
                event=json.loads(c.recv())
                if event.get('message')=='AudioAdded':
                    seq=event.get('seq_no')
                    with lock:
                        if seq in seen:
                            duplicates.append(seq); raise RuntimeError('duplicate AudioAdded '+str(seq))
                        if seq not in pending: raise RuntimeError('unexpected AudioAdded '+str(seq))
                        sent,label=pending.pop(seq);seen.add(seq)
                    audio_records.append({'stage':label,'connection':connection_id,'sequence':seq,'roundtrip_ms':round((time.perf_counter()-sent)*1000,3)})
                elif event.get('message')=='EndOfTranscript':
                    ended.set();return
                elif event.get('type')=='error' or event.get('message')=='Error': raise RuntimeError(str(event))
        except Exception as e:
            failed.set();errors.append({'stage':stage,'kind':'audio','connection':connection_id,'error':str(e)})
    try:
        c=websocket.create_connection(base.replace('http:','ws:')+'/ws/speechmatics?session_id='+sid,header=['Authorization: Bearer '+token],timeout=20,suppress_origin=True)
        c.send(json.dumps({'message':'StartRecognition','audio_format':{'type':'raw','encoding':'pcm_s16le','sample_rate':16000},'transcription_config':{'language':'en'}}))
        while True:
            event=json.loads(c.recv())
            if event.get('message')=='RecognitionStarted':break
            if event.get('type')=='error' or event.get('message')=='Error':raise RuntimeError(str(event))
        receiver=threading.Thread(target=receive,daemon=True);receiver.start()
        next_send=time.perf_counter()
        # One paced writer continues capturing synthetic PCM independently of ACKs.
        # A separate reader measures any queueing instead of throttling capture to ACK rate.
        while not done.is_set() and not failed.is_set():
            sequence+=1
            with lock: pending[sequence]=(time.perf_counter(),stage)
            c.send_binary(bytes(6400))
            next_send+=.2;done.wait(max(0,next_send-time.perf_counter()))
        if failed.is_set():raise RuntimeError('audio receiver failed')
        c.send(json.dumps({'message':'EndOfStream','last_seq_no':sequence}))
        if not ended.wait(20):raise RuntimeError('EndOfTranscript missing')
        with lock:
            if pending or len(seen)!=sequence:raise RuntimeError('missing AudioAdded acknowledgements')
    except Exception as e:errors.append({'stage':stage,'kind':'audio','connection':connection_id,'error':str(e)})
    finally:
        if c:c.close()
        if receiver:receiver.join(5)
        with lock:audio_connections.append({'connection':connection_id,'sent':sequence,'acknowledged':len(seen),'pending':sorted(pending),'duplicates':duplicates,'ended':ended.is_set(),'receiver_stopped':receiver is None or not receiver.is_alive()})
def start_audio():
    done=threading.Event();thread=threading.Thread(target=audio,args=(done,),daemon=True);thread.start();tasks.append((thread,done));return thread,done
def finish_audio(pair):
    thread,done=pair;done.set();thread.join(25)
    if thread.is_alive():raise RuntimeError('audio worker failed to exit')
def snapshot_stage(label,moment):
    s=state();applications={}
    for color in s['colors']:
        info=inspect(prefix+'-'+color)
        applications[color]={'container_id':info['Id'],'image_id':info['Image'],'running':info['State']['Running']}
    value={'moment':moment,'active':s['active'],'previous':s['previous'],'phase':s['phase'],'applications':applications}
    report['stage_applications'].setdefault(label,[]).append(value)
    return value
def set_stage(label):
    global stage
    stage=label;print(label,flush=True);return snapshot_stage(label,'start')
def wait_phase(seconds,label):
    value=set_stage(label)
    expected=report['image_ids']['old' if label=='old_active' else 'new']
    assert value['applications'][value['active']]['image_id']==expected, 'wrong active image in '+label
    time.sleep(seconds)

try:
    run('openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(ROOT/'key.pem'),'-out',str(ROOT/'cert.pem'),'-days','1','-subj','/CN=mp.speechmatics.com','-addext','subjectAltName=DNS:mp.speechmatics.com,DNS:global.rt.speechmatics.com')
    os.chmod(ROOT/'key.pem',0o600)
    for vol in volumes:run('docker','volume','create',vol)
    run('docker','network','create',network)
    run('docker','run','-d','--name',db,'--network',network,'--network-alias','db','--cpus','2','--memory','1536m','--mount',f'type=volume,src={volumes[0]},dst=/var/lib/postgresql/data','-e','POSTGRES_USER=fixture','-e','POSTGRES_DB=fixture','-e','POSTGRES_PASSWORD=fixture',PG)
    for _ in range(120):
        p=subprocess.run(['docker','exec',db,'pg_isready','-h','127.0.0.1','-U','fixture'],capture_output=True)
        if p.returncode==0:break
        time.sleep(.5)
    else:raise RuntimeError('DB not ready')
    for label,image in [('old',OLD),('new',NEW)]:
        x=run('docker','create',image)
        try:run('docker','cp',x+':/usr/share/dreamtrans/.',str(runroot/label))
        finally:run('docker','rm','-v',x)
    report['image_ids']={'old':inspect(OLD)['Id'],'new':inspect(NEW)['Id']}
    (ROOT/'dreamtransctl').write_bytes((runroot/'new/dreamtransctl').read_bytes());os.chmod(ROOT/'dreamtransctl',0o700)
    run('docker','run','--rm','--network',network,'--mount',f'type=bind,src={runroot}/old,dst=/release,readonly','-e','PGHOST=db','-e','PGUSER=fixture','-e','PGPASSWORD=fixture','-e','PGDATABASE=fixture','-e','MIGRATIONS_DIR=/release/migrations','--entrypoint','/bin/sh',PG,'/release/migrate.sh')
    run('docker','run','--rm','--user','0:0','--entrypoint','/bin/sh','--mount',f'type=volume,src={volumes[1]},dst=/data','--mount',f'type=bind,src={ROOT}/cert.pem,dst=/fixture.pem,readonly',OLD,'-ec','cp /fixture.pem /data/fixture-ca.pem; chown -R 10001:10001 /data')
    run('docker','run','-d','--name',provider,'--network',network,'--network-alias','mp.speechmatics.com','--network-alias','global.rt.speechmatics.com','--mount',f'type=bind,src={ROOT},dst=/fixture,readonly','--entrypoint','/fixture/provider','alpine:3.22')
    env=['DATABASE_URL=postgres://fixture:fixture@db:5432/fixture?sslmode=disable','JWT_SECRET='+key,'JWT_REFRESH_SECRET=fedcba9876543210fedcba9876543210','ALLOW_ANONYMOUS_API=false','SM_API_KEY=fixture','SSL_CERT_FILE=/app/data/fixture-ca.pem','PORT=8080','APP_BASE_URL=https://main.example.test','EDGE_SIGNING_SEED='+base64.b64encode(bytes(32)).decode().rstrip('='),'EDGE_ROUTING_ENABLED=false']
    args=['docker','run','-d','--name',legacy,'--network',network,'--network-alias','dreamtrans','-p','127.0.0.1::8080','--mount',f'type=volume,src={volumes[1]},dst=/app/data']
    for e in env:args+=['-e',e]
    run(*args,OLD)
    for _ in range(90):
        p=subprocess.run(['docker','exec',legacy,'wget','-qO-','http://127.0.0.1:8080/readyz'],capture_output=True)
        if p.returncode==0:break
        time.sleep(1)
    else:raise RuntimeError('legacy not ready')
    port=inspect(legacy)['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort']
    cli('init','--app',legacy,'--database',db,'--database-network',network,'--port',port,'--image',OLD,'--proxy-image',PROXY,'--maintenance')
    base='http://127.0.0.1:'+port
    sql(f"INSERT INTO tenants(id,name,slug) VALUES('{tid}','fixture','{tid}'); INSERT INTO users(id,tenant_id,email,name,password_hash,role,email_verified) VALUES('{uid}','{tid}','fixture@example.test','Fixture','unused','super_admin',true); INSERT INTO sessions(id,user_id,tenant_id) VALUES('{sid}','{uid}','{tid}'); INSERT INTO usage_logs(tenant_id,user_id,session_id,action,quantity,charge_usd,funding_route) SELECT '{tid}','{uid}','{sid}','transcription',1,.001,'paid' FROM generate_series(1,100000); ANALYZE usage_logs;")
    # Lazily create the synthetic wallet using the normal endpoint, then fund only it.
    assert request('/api/user/billing/account')==200, 'synthetic authentication/account setup failed'
    sql(f"UPDATE billing_accounts SET wallet_usd=100 WHERE owner_id='{uid}'")
    run('docker','update','--cpus','2',prefix+'-blue')
    loader=threading.Thread(target=load,daemon=True);loader.start()
    pair=start_audio();wait_phase(20,'old_active')
    set_stage('upgrade_to_new')
    cli('deploy','--image',NEW,'--observe','2','--drain-timeout','0')
    snapshot_stage(stage,'after_deploy')
    run('docker','update','--cpus','2',prefix+'-green')
    wait_phase(20,'new_http_old_audio_draining')
    finish_audio(pair);cli('drain','--drain-timeout','30')
    pair=start_audio();wait_phase(20,'new_active_before_config')
    set_stage('configure_edge_same_image')
    cli('configure-edge','--image',EDGE,'--proxy-image',PROXY,'--main','https://main.example.test','--observe','2','--drain-timeout','0')
    snapshot_stage(stage,'after_configure')
    run('docker','update','--cpus','2',prefix+'-blue')
    wait_phase(20,'new_http_new_audio_draining')
    finish_audio(pair);cli('drain','--drain-timeout','30')
    pair=start_audio();wait_phase(30,'new_active_after_config')
    finish_audio(pair);stop.set();loader.join(30)
    assert not loader.is_alive(), 'HTTP load worker did not exit'
    billing=sql(f"SELECT count(*),count(DISTINCT idempotency_key),coalesce(sum(charge_usd),0) FROM usage_logs WHERE user_id='{uid}' AND idempotency_key LIKE 'speechmatics:%'").split('|')
    report['billing_rows']=int(billing[0]);report['billing_distinct_keys']=int(billing[1]);report['billing_charge_usd']=billing[2]
    assert report['billing_rows']>0 and report['billing_rows']==report['billing_distinct_keys'] and float(billing[2])>0, 'no verified synthetic audio billing'
    report['final_state']={k:state()[k] for k in ['phase','active','previous']}
    report['cutover_http_failures']=[r for r in records if r['stage'] in cutover_stages and r['status']!=200]
    assert not [e for e in errors if e['kind']=='audio' or e['stage'] not in cutover_stages], 'unexpected audio/stable HTTP errors'
    for label in stable_stages:
        samples=[r for r in records if r['stage']==label]
        assert samples and all(r['status']==200 for r in samples), 'failed HTTP in stable stage '+label
        assert any(r['stage']==label for r in audio_records), 'missing audio samples in '+label
        assert any(r['path']=='/api/prompts/defaults' for r in samples), 'prompts path untested in '+label
    assert len(audio_connections)==3 and all(c['sent']>0 and c['sent']==c['acknowledged'] and not c['pending'] and not c['duplicates'] and c['ended'] and c['receiver_stopped'] for c in audio_connections), 'incomplete audio acknowledgements'
    assert report['final_state']['phase']=='ready', 'release did not complete draining'
    report['valid']=True
    print('experiment_complete',flush=True)
finally:
    stop.set()
    for thread,done in tasks:done.set();thread.join(25)
    report['http_raw']=records;report['audio_raw']=audio_records;report['audio_connections']=audio_connections;report['errors']=errors
    report['http_summary']=[]
    for label,path in sorted({(r['stage'],r['path']) for r in records}):
        chosen=[r for r in records if r['stage']==label and r['path']==path];v=sorted(r['total_ms'] for r in chosen)
        report['http_summary'].append({'stage':label,'path':path,'n':len(v),'statuses':{str(s):sum(r['status']==s for r in chosen) for s in sorted({r['status'] for r in chosen})},'p50_ms':statistics.median(v),'p95_ms':v[max(0,int(.95*len(v))-1)],'max_ms':max(v)})
    report['audio_summary']=[]
    for label in sorted({r['stage'] for r in audio_records}):
        v=sorted(r['roundtrip_ms'] for r in audio_records if r['stage']==label)
        report['audio_summary'].append({'stage':label,'n':len(v),'p50_ms':statistics.median(v),'p95_ms':v[max(0,int(.95*len(v))-1)],'max_ms':max(v)})
    (ROOT/'results.json').write_text(json.dumps(report,indent=2)+'\n')
    for name in containers:
        p=subprocess.run(['docker','logs',name],capture_output=True,text=True)
        if p.returncode==0:(ROOT/(name+'.log')).write_text(p.stdout+p.stderr)
        subprocess.run(['docker','rm','-fv',name],capture_output=True)
    for volume in volumes:subprocess.run(['docker','volume','rm',volume],capture_output=True)
    for network in networks:subprocess.run(['docker','network','rm',network],capture_output=True)
