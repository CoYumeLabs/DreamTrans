import base64, concurrent.futures, hashlib, hmac, json, os, statistics, subprocess, tempfile, time, urllib.request, uuid
from pathlib import Path

def run(*a):
    p=subprocess.run(a,text=True,capture_output=True,timeout=120)
    if p.returncode: raise RuntimeError(p.stderr[-1500:])
    return p.stdout.strip()
def inspect(n): return json.loads(run('docker','inspect',n))[0]
old='ghcr.io/coyumelabs/dreamtrans@sha256:d32344e29bff71713ccfa39ea19f646d95944dead09f0b754fb6202516462b90'
new='ghcr.io/coyumelabs/dreamtrans@sha256:a6fcde016bec5d69d78f4bdf0fcdbb9bf85df3c6755f4c1c6b529bbb3661b64d'
name='dt-compare-'+uuid.uuid4().hex[:8]; db=name+'-db'; containers=[db]; network=name; ports={}
key='0123456789abcdef0123456789abcdef'
uid,tid,sid=[str(uuid.uuid4()) for _ in range(3)]
try:
    run('docker','network','create',network)
    run('docker','run','-d','--name',db,'--network',network,'--network-alias','db','--tmpfs','/var/lib/postgresql/data','-e','POSTGRES_USER=fixture','-e','POSTGRES_PASSWORD=fixture','-e','POSTGRES_DB=fixture','pgvector/pgvector:0.8.2-pg16-bookworm')
    for _ in range(60):
        if subprocess.run(['docker','exec',db,'pg_isready','-U','fixture'],capture_output=True).returncode==0:break
        time.sleep(.5)
    with tempfile.TemporaryDirectory(prefix='dt-compare-') as d:
        x=run('docker','create',new)
        try:run('docker','cp',x+':/usr/share/dreamtrans/.',d)
        finally:run('docker','rm','-v',x)
        run('docker','run','--rm','--network',network,'--mount',f'type=bind,src={d},dst=/release,readonly','-e','PGHOST=db','-e','PGUSER=fixture','-e','PGPASSWORD=fixture','-e','PGDATABASE=fixture','-e','MIGRATIONS_DIR=/release/migrations','--entrypoint','/bin/sh','pgvector/pgvector:0.8.2-pg16-bookworm','/release/migrate.sh')
    query=f"INSERT INTO deployment_metadata(key,value) VALUES('server_config','{{}}'),('legacy_import_complete','true'); INSERT INTO tenants(id,name,slug) VALUES('{tid}','fixture','{tid}'); INSERT INTO users(id,tenant_id,email,name,password_hash,role,email_verified) VALUES('{uid}','{tid}','perf@example.test','Fixture','unused','super_admin',true); INSERT INTO sessions(id,user_id,tenant_id) VALUES('{sid}','{uid}','{tid}'); INSERT INTO usage_logs(tenant_id,user_id,session_id,action,quantity,charge_usd,funding_route) SELECT '{tid}','{uid}','{sid}','transcription',1,.001,'paid' FROM generate_series(1,100000); ANALYZE usage_logs;"
    run('docker','exec',db,'psql','-X','-v','ON_ERROR_STOP=1','-U','fixture','-d','fixture','-c',query)
    for label,ref in [('old',old),('new',new)]:
        cname=name+'-'+label; containers.append(cname)
        env=['DATABASE_URL=postgres://fixture:fixture@db:5432/fixture?sslmode=disable','JWT_SECRET='+key,'JWT_REFRESH_SECRET=fedcba9876543210fedcba9876543210','SM_API_KEY=fixture','PORT=8080','RAG_STORAGE=postgres','ALLOW_ANONYMOUS_API=false','EDGE_SIGNING_SEED='+base64.b64encode(bytes(32)).decode().rstrip('='),'EDGE_ROUTING_ENABLED=false','APP_BASE_URL=https://main.example.test']
        args=['docker','run','-d','--name',cname,'--network',network,'--cpus','2','-p','127.0.0.1::8080']
        for e in env:args += ['-e',e]
        run(*args,ref)

        if '8080/tcp' not in inspect(cname)['NetworkSettings']['Ports']:
            raise RuntimeError(subprocess.run(['docker','logs',cname],stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True).stdout)
        ports[label]=inspect(cname)['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort']
        for _ in range(60):
            try:
                with urllib.request.urlopen('http://127.0.0.1:'+ports[label]+'/readyz',timeout=1) as r: assert r.status==200
                break
            except Exception:time.sleep(.5)
        else:raise RuntimeError(label+' failed readiness '+subprocess.run(['docker','logs',cname],stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True).stdout)
    def enc(v):return base64.urlsafe_b64encode(json.dumps(v,separators=(',',':')).encode()).decode().rstrip('=')
    data=enc({'alg':'HS256','typ':'JWT'})+'.'+enc({'user_id':uid,'tenant_id':tid,'email':'perf@example.test','role':'super_admin','token_type':'access','iss':'dreamtrans','jti':str(uuid.uuid4()),'sub':uid,'exp':int(time.time())+600,'iat':int(time.time())-1,'nbf':int(time.time())-1})
    token=data+'.'+base64.urlsafe_b64encode(hmac.new(key.encode(),data.encode(),hashlib.sha256).digest()).decode().rstrip('=')
    paths=['/api/system/settings','/api/system/access','/api/user/profile','/api/admin/access','/api/user/billing/session-costs?session_ids='+sid]
    results={label:{p:[] for p in paths} for label in ports}
    def request(item):
        label,path=item; begin=time.perf_counter()
        req=urllib.request.Request('http://127.0.0.1:'+ports[label]+path,headers={'Authorization':'Bearer '+token})
        with urllib.request.urlopen(req,timeout=20) as r:assert r.status==200; r.read()
        return label,path,(time.perf_counter()-begin)*1000
    for iteration in range(3):
        jobs=[(label,path) for _ in range(20) for path in paths for label in (['old','new'] if iteration%2==0 else ['new','old'])]
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            for label,path,ms in pool.map(request,jobs):results[label][path].append(ms)
    report=[]
    for path in paths:
        row={'path':path.split('?')[0]}
        for label in ports:
            values=sorted(results[label][path]); row[label]={'n':len(values),'p50_ms':round(statistics.median(values)),'p95_ms':round(values[int(len(values)*.95)-1]),'max_ms':round(max(values))}
        report.append(row)
    print(json.dumps(report,indent=2),flush=True)
finally:
    for cname in containers[::-1]:subprocess.run(['docker','rm','-fv',cname],capture_output=True)
    subprocess.run(['docker','network','rm',network],capture_output=True)
