import base64, concurrent.futures, hashlib, hmac, json, subprocess, tempfile, time, urllib.request, urllib.error, uuid
from pathlib import Path

ROOT=Path('/tmp/dt-registration-db-wait-evidence')
ROOT.mkdir(exist_ok=True)
IMAGES={'old':'ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90','new':'ghcr.io/coyumelabs/dreamtrans@sha256:a6fcde016bec5d69d78f4bdf0fcdbb9bf85df3c6755f4c1c6b529bbb3661b64d'}
name='dt-db-wait-'+uuid.uuid4().hex[:8]; db=name+'-db'; network=name; containers=[db]
key='0123456789abcdef0123456789abcdef'; uid,tid,node=[str(uuid.uuid4()) for _ in range(3)]; registration=node+'.synthetic-registration-secret'
report={'images':IMAGES,'mode':'active','stages':[]}
def run(*args):
    p=subprocess.run(args,text=True,capture_output=True,timeout=120)
    if p.returncode: raise RuntimeError(p.stderr[-2000:])
    return p.stdout.strip()
def sql(query): return run('docker','exec',db,'psql','-X','-v','ON_ERROR_STOP=1','-At','-U','fixture','-d','fixture','-c',query)
def enc(v): return base64.urlsafe_b64encode(json.dumps(v,separators=(',',':')).encode()).decode().rstrip('=')
data=enc({'alg':'HS256','typ':'JWT'})+'.'+enc({'user_id':uid,'tenant_id':tid,'email':'perf@example.test','role':'super_admin','token_type':'access','iss':'dreamtrans','jti':str(uuid.uuid4()),'sub':uid,'exp':int(time.time())+600,'iat':int(time.time())-1,'nbf':int(time.time())-1})
token=data+'.'+base64.urlsafe_b64encode(hmac.new(key.encode(),data.encode(),hashlib.sha256).digest()).decode().rstrip('=')
def request(port,path,payload=None,kind=None):
    begin=time.perf_counter(); headers={'Authorization':'Bearer '+token}
    if payload is not None: headers['Content-Type']='application/json'
    req=urllib.request.Request('http://127.0.0.1:'+port+path,data=None if payload is None else json.dumps(payload).encode(),headers=headers)
    try:
        with urllib.request.urlopen(req,timeout=15) as r: code=r.status; r.read()
    except urllib.error.HTTPError as e: code=e.code; e.read()
    return {'kind':kind or path,'status':code,'milliseconds':round((time.perf_counter()-begin)*1000,2)}
def block():
    proc=subprocess.Popen(['docker','exec',db,'psql','-X','-v','ON_ERROR_STOP=1','-At','-U','fixture','-d','fixture','-c',f"SET application_name='dt_wait_blocker'; BEGIN; UPDATE edge_nodes SET name=name WHERE id='{node}'; SELECT pg_sleep(6); ROLLBACK;"],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
    for _ in range(100):
        if sql("SELECT count(*) FROM pg_stat_activity WHERE application_name='dt_wait_blocker' AND wait_event='PgSleep'")=='1': return proc
        time.sleep(.02)
    raise RuntimeError('blocker did not acquire row lock')
try:
    run('docker','network','create',network)
    run('docker','run','-d','--name',db,'--network',network,'--network-alias','db','--tmpfs','/var/lib/postgresql/data','-e','POSTGRES_USER=fixture','-e','POSTGRES_PASSWORD=fixture','-e','POSTGRES_DB=fixture','pgvector/pgvector:0.8.2-pg16-bookworm')
    for _ in range(120):
        # TCP readiness skips PostgreSQL image's temporary Unix-only init server.
        if subprocess.run(['docker','exec',db,'pg_isready','-h','127.0.0.1','-U','fixture','-d','fixture'],capture_output=True).returncode==0: break
        time.sleep(.25)
    else: raise RuntimeError('database not ready')
    with tempfile.TemporaryDirectory(prefix='dt-db-wait-release-') as release:
        cid=run('docker','create',IMAGES['new'])
        try: run('docker','cp',cid+':/usr/share/dreamtrans/.',release)
        finally: run('docker','rm','-v',cid)
        run('docker','run','--rm','--network',network,'--mount',f'type=bind,src={release},dst=/release,readonly','-e','PGHOST=db','-e','PGUSER=fixture','-e','PGPASSWORD=fixture','-e','PGDATABASE=fixture','-e','MIGRATIONS_DIR=/release/migrations','--entrypoint','/bin/sh','pgvector/pgvector:0.8.2-pg16-bookworm','/release/migrate.sh')
    sql(f"INSERT INTO deployment_metadata(key,value) VALUES('server_config','{{}}'),('legacy_import_complete','true'); INSERT INTO tenants(id,name,slug) VALUES('{tid}','fixture','{tid}'); INSERT INTO users(id,tenant_id,email,name,password_hash,role,email_verified) VALUES('{uid}','{tid}','perf@example.test','Fixture','unused','super_admin',true); INSERT INTO edge_nodes(id,name,region,endpoint,max_connections,registration_hash,registration_until) VALUES('{node}','Fixture Node','fixture','https://edge.example.test',2,'{hashlib.sha256(registration.encode()).hexdigest()}',now()+interval '15 minutes');")
    for label,ref in IMAGES.items():
        cname=name+'-'+label; containers.append(cname)
        env=['DATABASE_URL=postgres://fixture:fixture@db:5432/fixture?sslmode=disable','JWT_SECRET='+key,'JWT_REFRESH_SECRET=fedcba9876543210fedcba9876543210','SM_API_KEY=fixture','SM_API_KEY_NO_TRAINING=fixture-private','PORT=8080','RAG_STORAGE=postgres','ALLOW_ANONYMOUS_API=false','EDGE_SIGNING_SEED='+base64.b64encode(bytes(32)).decode().rstrip('='),'EDGE_ROUTING_ENABLED=false','APP_BASE_URL=https://main.example.test','DREAMTRANS_DEPLOYMENT_MODE=active','DREAMTRANS_DEPLOYMENT_STATE=/tmp/dt-deployment-mode']
        args=['docker','run','-d','--name',cname,'--network',network,'--cpus','2','-p','127.0.0.1::8080']
        for item in env: args+=['-e',item]
        run(*args,ref)
        info=json.loads(run('docker','inspect',cname))[0]; port=info['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort']
        for _ in range(100):
            try:
                if request(port,'/readyz')['status']==200: break
            except Exception: pass
            time.sleep(.1)
        else: raise RuntimeError(label+' application failed readiness')
        report.setdefault('deployment_status',{})[label]=json.loads(run('docker','exec',cname,'/app/server','deploy-control','status'))
        baseline=[request(port,p) for p in ['/healthz','/readyz','/api/system/settings','/api/system/access','/api/user/profile']]
        baseline+=[request(port,'/api/edge-control/register',{'token':''},'empty_registration'),request(port,'/api/edge-control/register',{'token':registration},'valid_registration')]
        report['stages'].append({'image':label,'stage':'baseline','requests':baseline})
        for count in [1,25]:
            blocker=block()
            with concurrent.futures.ThreadPoolExecutor(max_workers=35) as pool:
                pending=[pool.submit(request,port,'/api/edge-control/register',{'token':registration},'valid_registration') for _ in range(count)]
                time.sleep(.3)
                observations=[pool.submit(request,port,p) for p in ['/healthz','/readyz','/api/system/settings','/api/system/access','/api/user/profile']]
                observations.append(pool.submit(request,port,'/api/edge-control/register',{'token':''},'empty_registration'))
                wait_snapshot=sql("SELECT coalesce(wait_event_type,'none'),coalesce(wait_event,'none'),count(*) FROM pg_stat_activity WHERE datname='fixture' AND state='active' AND pid<>pg_backend_pid() GROUP BY 1,2 ORDER BY 1,2")
                results=[f.result() for f in observations+pending]
            out,err=blocker.communicate(timeout=10)
            if blocker.returncode: raise RuntimeError(err)
            report['stages'].append({'image':label,'stage':f'row_lock_{count}_registrations','pg_wait_snapshot':wait_snapshot,'requests':results})
        (ROOT/(label+'-app.log')).write_text(run('docker','logs',cname))
    (ROOT/'result.json').write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps(report,indent=2),flush=True)
finally:
    for cname in containers[::-1]: subprocess.run(['docker','rm','-fv',cname],capture_output=True)
    subprocess.run(['docker','network','rm',network],capture_output=True)
