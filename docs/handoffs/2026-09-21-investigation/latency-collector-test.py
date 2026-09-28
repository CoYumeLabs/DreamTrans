import json, os, pathlib, shutil, stat, subprocess, tempfile, time, uuid
root=pathlib.Path(tempfile.mkdtemp(prefix='dtcollector-test-'))
root.chmod(0o700)
prefix='dtcollector-test-'+uuid.uuid4().hex[:8]
network=prefix+'-entry'
app=prefix+'-blue'; proxy=prefix+'-proxy'; db=prefix+'-db'
volumes=[prefix+'-app',prefix+'-pg']
secret="SENTINEL_MUST_NOT_APPEAR_'$_`_\\_"+uuid.uuid4().hex
body='BODY_SENTINEL_MUST_NOT_APPEAR'
real_docker=shutil.which('docker'); real_curl=shutil.which('curl')
script='/tmp/dt-latency-readonly-20260921.sh'
records=[]

def run(args, **kw):
    result=subprocess.run(args,text=True,capture_output=True,**kw)
    if result.returncode:
        raise AssertionError(f'command failed ({args[0:2]}), exit {result.returncode}; stderr withheld')
    return result.stdout.strip()
def docker(*args):return run([real_docker,*args])
def inspect(name):return json.loads(docker('inspect',name))[0]
def save_state(state):
    p=root/'.bluegreen/state.json';p.write_text(json.dumps(state));p.chmod(0o600)
def collect(*extra):
    return subprocess.run([script,'--dir',str(root),'--rounds','1','--interval','1',*extra],env=env,text=True,capture_output=True,timeout=75)
def outputs(result):
    assert result.returncode==0, f'collector failed: {result.stderr}'
    out=pathlib.Path(result.stdout.strip().splitlines()[-1]); assert out.is_dir()
    assert stat.S_IMODE(out.stat().st_mode)==0o700
    for p in out.rglob('*'):
        if p.is_file():
            text=p.read_text();assert secret not in text,(p,'secret leaked');assert body not in text,(p,'body leaked')
            assert not (stat.S_IMODE(p.stat().st_mode)&0o077),str(p)
    assert secret not in result.stdout+result.stderr
    records.append(str(out));return out
try:
    (root/'.bluegreen/proxy').mkdir(parents=True)
    (root/'app').mkdir()
    (root/'app/nginx.conf').write_text('events {}\nhttp {server {listen 8080;location / {default_type text/plain; return 200 "'+body+'";}}}\n')
    (root/'.bluegreen/proxy/nginx.conf').write_text('events {}\nhttp {server {listen 8080;location = /_release {return 200 "blue";}location / {proxy_pass http://'+app+':8080;}}}\n')
    docker('network','create',network)
    for volume in volumes:docker('volume','create',volume)
    envfile=root/'fixture.env';envfile.write_text('POSTGRES_USER=collector\nPOSTGRES_DB=collector\nPOSTGRES_PASSWORD='+secret+'\n');envfile.chmod(0o600)
    docker('run','-d','--name',db,'--network',network,'--env-file',str(envfile),'-v',volumes[1]+':/var/lib/postgresql/data','postgres:16-alpine')
    for _ in range(60):
        check=subprocess.run([real_docker,'exec',db,'pg_isready','-U','collector','-d','collector'],capture_output=True)
        if check.returncode==0:break
        time.sleep(.5)
    else:raise AssertionError('disposable PG readiness failed')
    docker('run','-d','--name',app,'--network',network,'--label','dreamtrans.release='+prefix,'-v',volumes[0]+':/app/data','-v',str(root/'app/nginx.conf')+':/etc/nginx/nginx.conf:ro','nginx:1.27-alpine')
    docker('run','-d','--name',proxy,'--network',network,'-p','127.0.0.1::8080','-v',str(root/'.bluegreen/proxy')+':/release:ro','nginx:1.27-alpine','nginx','-g','daemon off;','-c','/release/nginx.conf')
    a,d,p=inspect(app),inspect(db),inspect(proxy)
    state={'format':1,'prefix':prefix,'active':'blue','phase':'ready','port':int(p['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort']),'network':network,'database_id':d['Id'],'database_volume':volumes[1],'application_volume':volumes[0],'database_image':d['Image'],'proxy_image':p['Image'],'colors':{'blue':{'image':a['Image']}},'application_env':{'APP_BASE_URL':'https://fixture.example','TOKEN_SHOULD_NOT_PRINT':secret},'database_env':{'PGHOST':db,'PGUSER':'collector','PGDATABASE':'collector','PGPASSWORD':secret,'PGPORT':'5432'}}
    save_state(state)
    wrappers=root/'bin';wrappers.mkdir()
    log=root/'commands.jsonl'
    docker_wrapper='''#!/usr/bin/env python3
import json,os,subprocess,sys
args=sys.argv[1:]
assert args and args[0] in ['inspect','stats','exec'], 'mutation forbidden'
assert 'deploy-control' not in ' '.join(args), 'admission action forbidden'
assert os.environ['TEST_SECRET'] not in ' '.join(args), 'secret in argv'
with open(os.environ['TEST_LOG'],'a') as f:f.write(json.dumps(['docker',*args])+'\\n')
sys.exit(subprocess.call([os.environ['REAL_DOCKER'],*args]))
'''
    curl_wrapper='''#!/usr/bin/env python3
import json,os,subprocess,sys
args=sys.argv[1:]
assert args[0]=='-q'
assert '--noproxy' in args and '--max-time' in args
assert '-L' not in args and '--location' not in args
assert os.environ['TEST_SECRET'] not in ' '.join(args)
with open(os.environ['TEST_LOG'],'a') as f:f.write(json.dumps(['curl',*args])+'\\n')
if args[-1].startswith('https://fixture.example/'):
 print('000\\t0\\t\\t0.000010\\t0.000000\\t0.000000\\t0.000000\\t0.000000\\t0.001000\\t0')
 sys.exit(28)
sys.exit(subprocess.call([os.environ['REAL_CURL'],*args]))
'''
    for name,text in [('docker',docker_wrapper),('curl',curl_wrapper)]:
        file=wrappers/name;file.write_text(text);file.chmod(0o700)
    env=dict(os.environ,PATH=str(wrappers)+':'+os.environ['PATH'],TEST_LOG=str(log),TEST_SECRET=secret,REAL_DOCKER=real_docker,REAL_CURL=real_curl)
    out=outputs(collect('--no-public'))
    rows=(out/'http.tsv').read_text().splitlines();assert len(rows)==9,rows
    assert all(line.split('\t')[3:5]==['0','200'] for line in rows[1:]),rows
    pg=json.loads((out/'round-1/postgres-status.json').read_text());assert pg['status']=='ok',pg
    db_rows=[json.loads(line) for line in (out/'round-1/postgres.jsonl').read_text().splitlines()]
    assert any(r['kind']=='visibility' and r['all_stats'] for r in db_rows),db_rows
    print('PASS: disposable real Docker PG/direct HTTP/proxy HTTP; 8 successful requests, read-only PG snapshots, private permissions, no secret/body leakage')
    out=outputs(collect('--public-url','https://fixture.example'))
    rows=(out/'http.tsv').read_text().splitlines()[1:]
    public=[r.split('\t') for r in rows if '\tpublic\t' in r]
    assert len(public)==4 and all(r[3:5]==['28','000'] for r in public),public
    print('PASS: mocked public timeout recorded as curl exit 28 and HTTP 000, never claimed as success')
    cases=[('wrong_database_volume',{'database_volume':prefix+'-WRONG'}),('malicious_prefix',{'prefix':'x; touch /tmp/NEVER'}),('invalid_active',{'active':'red'})]
    for title,change in cases:
        altered=dict(state,**change);save_state(altered);log.write_text('')
        result=collect('--no-public');assert result.returncode!=0,title
        calls=[json.loads(line) for line in log.read_text().splitlines()]
        assert not any(call[0]=='curl' or (call[0]=='docker' and call[1]=='exec') for call in calls),(title,calls)
        assert secret not in result.stderr+result.stdout,title
        print('PASS: '+title+' rejected before HTTP/SQL')
    save_state(state);log.write_text('')
    result=collect('--public-url','https://user:secret@fixture.example/x?token=secret');assert result.returncode!=0
    assert not log.read_text();print('PASS: URL credentials/path/query rejected before Docker/HTTP/SQL')
    altered=json.loads(json.dumps(state));altered['database_env']['PGPASSWORD']='unsupported\nmultiline';save_state(altered)
    out=outputs(collect('--no-public'));pg=json.loads((out/'round-1/postgres-status.json').read_text());assert pg['status']=='skipped'
    print('PASS: unsupported multiline credentials explicitly skip PG without executing credential payload')
    save_state(state)
    summary={'fixture':str(root),'outputs':records,'passed':['real_direct_and_proxy','read_only_postgres','stdin_secret','no_response_body','private_output','public_timeout','wrong_db_volume','malicious_prefix','bad_active','bad_public_url','multiline_credentials']}
    (root/'verification.json').write_text(json.dumps(summary,indent=2));print(json.dumps(summary,indent=2))
finally:
    for name in [proxy,app,db]:subprocess.run([real_docker,'rm','-f',name],capture_output=True)
    for volume in volumes:subprocess.run([real_docker,'volume','rm',volume],capture_output=True)
    subprocess.run([real_docker,'network','rm',network],capture_output=True)
