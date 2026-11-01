"""Build a local, tested, versioned artifact. Never installs or publishes."""
import hashlib,json,os,pathlib,re,subprocess
ROOT=pathlib.Path(__file__).resolve().parents[1]
def main():
 version=(ROOT/'VERSION').read_text().strip();assert re.fullmatch(r'[0-9][0-9A-Za-z.+-]*',version)
 src=(ROOT/'internal/kiro/dispatch.go').read_text();assert '"'+version+'"' in src,'Metadata/version mismatch'
 for cmd in [['go','test','-race','./...'],['go','vet','./...']]:subprocess.run(cmd,cwd=ROOT,check=True)
 info=json.loads(subprocess.check_output(['go','env','-json','GOOS','GOARCH','GOVERSION'],cwd=ROOT))
 ext={'darwin':'dylib','linux':'so','windows':'dll'}[info['GOOS']]
 out=ROOT/'dist'/version;out.mkdir(parents=True,exist_ok=True)
 artifact=out/f'kiro-v{version}.{ext}'
 subprocess.run(['go','build','-trimpath','-buildmode=c-shared','-o',str(artifact),'.'],cwd=ROOT,env=dict(os.environ,CGO_ENABLED='1'),check=True)
 manifest={'version':version,'provider':'kiro','upstream_base':'f7b60a605c3613c18e4011b922f93b5bad70522c','platform':info,'artifact':artifact.name,'sha256':hashlib.sha256(artifact.read_bytes()).hexdigest(),'source_commit':subprocess.check_output(['git','rev-parse','HEAD'],cwd=ROOT,text=True).strip(),'source_dirty':bool(subprocess.check_output(['git','status','--porcelain'],cwd=ROOT)),'tests':'go test -race ./...; go vet ./...','published':False}
 (out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n');print(json.dumps(manifest,indent=2))
if __name__=='__main__':main()
