#!/usr/bin/env python3
import argparse
import subprocess,json,hashlib,re
parser=argparse.ArgumentParser(description="Run deployed reflection probes on the two explicitly configured test nodes")
parser.add_argument("--known-hosts", required=True)
parser.add_argument("--output", default="/tmp/kubeovn-scale-reflection-result.json")
args=parser.parse_args()
ssh=['ssh','-F','/dev/null','-o','IdentityFile=none','-o','BatchMode=yes','-o','UserKnownHostsFile='+args.known_hosts]
def run(ip,cmd): return subprocess.check_output(ssh+['root@'+ip,cmd],text=True)
pods=json.loads(run('10.64.244.68','kubectl --kubeconfig=/etc/kubernetes/admin.conf -n kubeovn-scale-check get pods -o json'))['items']
macs={p['metadata']['name']:p['metadata']['annotations']['ovn.kubernetes.io/mac_address'] for p in pods}
subnet=json.loads(run('10.64.244.68','kubectl --kubeconfig=/etc/kubernetes/admin.conf get subnet pci-docker-z504 -o json'))
table='kopr_'+hashlib.sha256(subnet['metadata']['uid'].encode()).hexdigest()[:24]
root='/var/tmp/kubeovn-scaling-20261123'
results={}
for ip,pod,other in [('10.64.244.68','l2-1','l2-2'),('10.64.244.69','l2-2','l2-1')]:
 own,reflected=macs[pod],macs[other]; assert re.fullmatch('[0-9a-f:]{17}',reflected)
 command=f'''set -e
sandbox=$(crictl --runtime-endpoint=unix:///run/containerd/containerd.sock pods --namespace kubeovn-scale-check --name {pod} -q)
pid=$(crictl --runtime-endpoint=unix:///run/containerd/containerd.sock inspectp "$sandbox" | python3 -c 'import sys,json;print(json.load(sys.stdin)["info"]["pid"])')
crictl --runtime-endpoint=unix:///run/containerd/containerd.sock exec "$(crictl --runtime-endpoint=unix:///run/containerd/containerd.sock ps --name cni-server -q)" nft -j list table netdev {table} > {root}/reflection-before.json
nsenter -t "$pid" -n python3 {root}/packet_probe.py listen --device eth0 --seconds 3 > {root}/reflection-final.json 2> {root}/reflection-final.err & receiver=$!
for i in $(seq 1 40); do if grep -q READY {root}/reflection-final.err; then break; fi; sleep 0.05; done
grep -q READY {root}/reflection-final.err
ip netns exec perouter python3 {root}/packet_probe.py send --device pe-e-504241 --source {reflected}
ip netns exec perouter python3 {root}/packet_probe.py send --device pe-e-504241 --source 02:aa:ab:ac:ad:ae --types 88b5,86dd,8035
wait "$receiver"
crictl --runtime-endpoint=unix:///run/containerd/containerd.sock exec "$(crictl --runtime-endpoint=unix:///run/containerd/containerd.sock ps --name cni-server -q)" nft -j list table netdev {table} > {root}/reflection-after.json
ip netns exec perouter python3 {root}/packet_probe.py listen --device pe-e-504241 --seconds 3 --marker OPR456 > {root}/outbound-rarp.json 2> {root}/outbound-rarp.err & receiver=$!
for i in $(seq 1 40); do if grep -q READY {root}/outbound-rarp.err; then break; fi; sleep 0.05; done
grep -q READY {root}/outbound-rarp.err
nsenter -t "$pid" -n python3 {root}/packet_probe.py send --device eth0 --source {own} --types 8035 --marker OPR456
wait "$receiver"
'''
 print(run(ip,command),flush=True)
 capture=json.loads(run(ip,f'cat {root}/reflection-final.json'))
 assert capture=={'02:aa:ab:ac:ad:ae/'+kind:1 for kind in ['88b5','86dd','8035']},capture
 rarp=json.loads(run(ip,f'cat {root}/outbound-rarp.json')); assert rarp=={own+'/8035':1},rarp
 def counter(suffix):
  dump=json.loads(run(ip,f'cat {root}/reflection-{suffix}.json'))
  return sum(expr.get('counter',{}).get('packets',0) for obj in dump['nftables'] for expr in obj.get('rule',{}).get('expr',[]))
 delta=counter('after')-counter('before'); assert delta>=4,delta
 results[ip]={'reflected_mac':reflected,'positive_controls':capture,'outbound_rarp':rarp,'reflected_drop_counter_delta':delta}
 print(ip,'PASS',results[ip],flush=True)
open(args.output,'w').write(json.dumps(results,indent=2)+'\n')
