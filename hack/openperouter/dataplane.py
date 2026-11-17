#!/usr/bin/env python3
"""Physical OVN component lab; does not claim Cisco/PE/Kubernetes e2e coverage."""
import json,os,pathlib,subprocess,tempfile,time,traceback
root=pathlib.Path(os.environ.get('KUBE_OVN_OPENPEROUTER_RUN_ROOT', '/tmp/kubeovn-openperouter-full'))
results=root/'results';config=tempfile.mkdtemp(prefix='kubeovn-opr-dp-docker-',dir='/tmp')
docker=['docker','--config',config,'--host','unix:///var/run/docker.sock']
image='kubeovn-openperouter-full-checks:20261116'
network='kubeovn-opr-dp-net'
names={n:'kubeovn-opr-dp-'+n for n in ['db','node-a','node-b']}
created=[];network_created=False;checks=[]

def run(arguments,check=True):
 result=subprocess.run(arguments,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
 if check and result.returncode:raise RuntimeError(f'{arguments!r}: {result.returncode}\n{result.stdout}\n{result.stderr}')
 return result.stdout.strip()

def d(*args,check=True):return run(docker+list(args),check)
def ex(node,*args,check=True):return d('exec',names[node],*args,check=check)
def sh(node,script):return ex(node,'/bin/bash','-euc',script)
def nb(*args):return ex('db','ovn-nbctl','--db=tcp:127.0.0.1:6641','--timeout=5',*args)
def sb(*args):return ex('db','ovn-sbctl','--db=tcp:127.0.0.1:6642','--timeout=5',*args)
def launch(node,ip,script):
 d('run','-d','--name',names[node],'--hostname',node,'--label','kubeovn.openperouter.validation=full-20261116','--network',network,'--ip',ip,'--privileged','--memory','512m','--cpus','0.5','--mount',f'type=bind,source={root},target=/lab,readonly',image,'-euc',script)
 created.append(node)
def wait(predicate,seconds=30):
 end=time.monotonic()+seconds
 while time.monotonic()<end:
  try:
   if predicate():return
  except Exception:pass
  time.sleep(.2)
 raise TimeoutError('condition not ready')

def capture(node,namespace,interface,mode,needle,label):
 output='/tmp/'+label+'.pcap'
 args=docker+['exec',names[node]]
 if namespace:args+=['ip','netns','exec',namespace]
 args+=['python3','/lab/source/hack/openperouter/packets.py','capture',interface,mode,needle,output]
 process=subprocess.Popen(args,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
 if process.stdout.readline().strip()!='READY':raise RuntimeError('capture not ready: '+process.stderr.read())
 return (node,process,output,label)
def finish(captures):
 output={}
 for node,process,path,label in captures:
  stdout,stderr=process.communicate(timeout=10)
  if process.returncode:raise RuntimeError(stderr)
  data=json.loads(stdout);output[label]=data['count']
  d('cp',names[node]+':'+path,str(results/(label+'.pcap')))
 return output

def record(name,callback):
 try:detail=callback();entry={'name':name,'status':'PASS','detail':detail}
 except Exception as error:entry={'name':name,'status':'FAIL','error':str(error),'trace':traceback.format_exc()}
 checks.append(entry);(results/'ovn-dataplane-checks.json').write_text(json.dumps(checks,indent=2))
 print(json.dumps(entry),flush=True)

def require(condition,detail):
 if not condition:raise AssertionError(detail)
 return detail

try:
 for name in names.values():
  result=subprocess.run(docker+['inspect',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
  if result.returncode==0:raise RuntimeError('Refuse pre-existing container '+name)
 d('network','create','--internal','--subnet','172.30.246.0/24','--label','kubeovn.openperouter.validation=full-20261116',network);network_created=True
 launch('db','172.30.246.10',r'''
 mkdir -p /var/run/ovn
 ovsdb-tool create /tmp/nb.db /usr/share/ovn/ovn-nb.ovsschema
 ovsdb-tool create /tmp/sb.db /usr/share/ovn/ovn-sb.ovsschema
 ovsdb-server --detach --pidfile=/tmp/nb.pid --unixctl=/tmp/nb.ctl --remote=ptcp:6641:0.0.0.0 /tmp/nb.db
 ovsdb-server --detach --pidfile=/tmp/sb.pid --unixctl=/tmp/sb.ctl --remote=ptcp:6642:0.0.0.0 /tmp/sb.db
 ovn-nbctl --db=tcp:127.0.0.1:6641 init
 ovn-sbctl --db=tcp:127.0.0.1:6642 init
 ovn-northd --detach --pidfile=/tmp/northd.pid --unixctl=/tmp/northd.ctl --ovnnb-db=tcp:127.0.0.1:6641 --ovnsb-db=tcp:127.0.0.1:6642
 exec sleep infinity
 ''')
 wait(lambda:bool(nb('get','NB_Global','.','_uuid')))
 ex('db','/lab/dataplane-helper','nb','tcp:127.0.0.1:6641')
 config_data=json.loads(ex('db','/lab/dataplane-helper','describe'))
 (results/'ovn-dataplane-topology.json').write_text(json.dumps(config_data,indent=2))
 transit=config_data['transit']
 for i,node in enumerate(['node-a','node-b']):
  desc=config_data[node];ip=f'172.30.246.{11+i}'
  script=f'''
 mkdir -p /var/run/openvswitch /var/run/ovn
 ovsdb-tool create /tmp/ovs.db /usr/share/openvswitch/vswitch.ovsschema
 ovsdb-server --detach --pidfile=/tmp/ovsdb.pid --unixctl=/tmp/ovsdb.ctl --remote=punix:/var/run/openvswitch/db.sock /tmp/ovs.db
 ovs-vsctl --no-wait init
 ovs-vsctl --no-wait set Open_vSwitch . external_ids:system-id={node} external_ids:hostname={node} external_ids:ovn-remote=tcp:172.30.246.10:6642 external_ids:ovn-encap-type=vxlan external_ids:ovn-encap-ip={ip}
 ovs-vsctl --no-wait add-br br-int
 ovs-vswitchd --detach --pidfile=/tmp/vswitchd.pid --unixctl=/tmp/vswitchd.ctl unix:/var/run/openvswitch/db.sock
 ovn-controller --detach --pidfile=/tmp/ovn.pid --unixctl=/tmp/ovn.ctl unix:/var/run/openvswitch/db.sock
 exec sleep infinity
 '''
  launch(node,ip,script)
  wait(lambda node=node:ex(node,'ovs-vsctl','br-exists','br-int')=='')
  for logical,iface,mac in [(transit['EgressPort'],desc['EgressInterface'],transit['EgressMAC']),(desc['ReturnPort'],desc['ReturnInterface'],desc['ReturnMAC'])]:
   sh(node,f'ovs-vsctl add-port br-int {iface} -- set Interface {iface} type=internal external_ids:iface-id={logical}; ip link set {iface} address {mac} up')
  suffix=['a','b'][i]
  for subnet,prefix,macpart in [('pods','10.101.3',3),('blue','10.101.1',1)]:
   ns=subnet;host='v'+subnet
   mac=f'02:00:00:00:{macpart:02x}:{17+i:02x}'
   sh(node,f'ip netns add {ns}; ip link add {host} type veth peer name eth0 netns {ns}; ip link set {host} mtu 1400 up; ip -n {ns} link set lo up; ip -n {ns} link set eth0 address {mac} mtu 1400 up; ip -n {ns} address add {prefix}.{11+i}/24 dev eth0; ip -n {ns} route add default via {prefix}.1; ovs-vsctl add-port br-int {host} -- set Interface {host} external_ids:iface-id={subnet}-{suffix}')
  for subnet,part in [('pods',3),('blue',1)]:
   sh(node,f'ip netns exec {subnet} sysctl -qw net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.default.disable_ipv6=0 net.ipv6.conf.eth0.disable_ipv6=0; ip -n {subnet} -6 address add fd00:101:{part}::{11+i}/64 dev eth0 nodad; ip -n {subnet} -6 route add default via fd00:101:{part}::1')
  sh(node,f'ip netns add fabric; ip link add vfab type veth peer name eth0 netns fabric; ip link set vfab up; ip -n fabric link set eth0 up; ovs-vsctl add-port br-int vfab -- set Interface vfab external_ids:iface-id={config_data["l2Port"]}')
 wait(lambda: len(sb('--bare','--columns=name','list','Chassis').split())==2)
 wait(lambda: 'true' in sb('get','Port_Binding','pods-a','up'))
 wait(lambda: 'true' in sb('get','Port_Binding','pods-b','up'))
 time.sleep(2)
 def east_west():
  output=ex('node-a','ip','netns','exec','pods','ping','-n','-c','3','-W','2','10.101.3.12')
  return require('0% packet loss' in output,output)
 record('same-vrf-internal-vxlan',east_west)
 def east_west6():
  return ex('node-a','ip','netns','exec','pods','ping','-6','-n','-c','2','-W','2','fd00:101:3::12')
 record('same-vrf-ipv6-nd-internal-vxlan',east_west6)

 for node,other,source in [('node-a','node-b','10.101.3.11'),('node-b','node-a','10.101.3.12')]:
  def egress(node=node,other=other,source=source):
   needle=source+',203.0.113.10';label='egress-'+node
   caps=[capture(node,None,config_data[node]['EgressInterface'],'icmp',needle,label+'-local'),capture(other,None,config_data[other]['EgressInterface'],'icmp',needle,label+'-other'),capture(node,None,'eth0','vxlan-icmp',needle,label+'-tunnel')]
   ex(node,'ip','netns','exec','pods','ping','-n','-c','2','-W','1','203.0.113.10',check=False)
   counts=finish(caps);return require(counts[label+'-local']>0 and counts[label+'-other']==0 and counts[label+'-tunnel']==0,counts)
  record('local-egress-'+node,egress)
 def return_path():
  needle='203.0.113.10,10.101.3.12';label='return'
  caps=[capture('node-b','pods','eth0','icmp',needle,label+'-pod'),capture('node-a',None,config_data['node-a']['EgressInterface'],'icmp',needle,label+'-egress-a'),capture('node-b',None,config_data['node-b']['EgressInterface'],'icmp',needle,label+'-egress-b')]
  ex('node-a','python3','/lab/source/hack/openperouter/packets.py','icmp',config_data['node-a']['ReturnInterface'],config_data['node-a']['ReturnMAC'],transit['RouterMAC'],'203.0.113.10','10.101.3.12')
  counts=finish(caps);return require(counts[label+'-pod']==1 and counts[label+'-egress-a']==0 and counts[label+'-egress-b']==0,counts)
 record('separate-return-remote-workload-no-loop',return_path)
 def return_fence():
  needle='203.0.113.10,203.0.113.20';label='return-fence'
  caps=[capture(n,None,config_data[n]['EgressInterface'],'icmp',needle,label+'-'+n) for n in ['node-a','node-b']]
  ex('node-a','python3','/lab/source/hack/openperouter/packets.py','icmp',config_data['node-a']['ReturnInterface'],config_data['node-a']['ReturnMAC'],transit['RouterMAC'],'203.0.113.10','203.0.113.20')
  counts=finish(caps)
  (results/'return-fence-acls.txt').write_text(nb('list','ACL'))
  (results/'return-fence-flows.txt').write_text(sb('lflow-list',transit['Switch']))
  flow=f'inport == "{config_data["node-a"]["ReturnPort"]}" && eth.src == {config_data["node-a"]["ReturnMAC"]} && eth.dst == {transit["RouterMAC"]} && ip4.src == 203.0.113.10 && ip4.dst == 203.0.113.20 && ip.ttl == 64 && ip.proto == 1 && icmp4.type == 8 && icmp4.code == 0'
  (results/'return-fence-trace.txt').write_text(ex('db','ovn-trace','--db=tcp:127.0.0.1:6642','--detailed',transit['Switch'],flow,check=False))
  return require(all(c==0 for c in counts.values()),counts)
 record('return-cannot-reenter-external-default',return_fence)
 def fence6():
  needle='2001:db8:203::10,2001:db8:203::20'
  caps=[capture(n,None,config_data[n]['EgressInterface'],'icmp6',needle,'return6-fence-'+n) for n in ['node-a','node-b']]
  ex('node-a','python3','/lab/source/hack/openperouter/packets.py','icmp6',config_data['node-a']['ReturnInterface'],config_data['node-a']['ReturnMAC'],transit['RouterMAC'],'2001:db8:203::10','2001:db8:203::20')
  counts=finish(caps);return require(all(c==0 for c in counts.values()),counts)
 record('ipv6-return-cannot-reenter-external-default',fence6)
 def return6():
  needle='2001:db8:203::10,fd00:101:3::12'
  caps=[capture('node-b','pods','eth0','icmp6',needle,'return6-pod')]
  ex('node-a','python3','/lab/source/hack/openperouter/packets.py','icmp6',config_data['node-a']['ReturnInterface'],config_data['node-a']['ReturnMAC'],transit['RouterMAC'],'2001:db8:203::10','fd00:101:3::12')
  counts=finish(caps);return require(counts['return6-pod']==1,counts)
 record('ipv6-return-remote-workload',return6)

 def bum():
  token='KUBEOVN-OPR-BUM-1'
  caps=[capture('node-a','fabric','eth0','custom',token,'bum-fabric-a'),capture('node-b','fabric','eth0','custom',token,'bum-fabric-b'),capture('node-b','blue','eth0','custom',token,'bum-pod-b')]
  ex('node-a','ip','netns','exec','blue','python3','/lab/source/hack/openperouter/packets.py','custom','eth0','02:00:00:00:01:11','ff:ff:ff:ff:ff:ff',token)
  counts=finish(caps);return require(counts['bum-fabric-a']==1 and counts['bum-fabric-b']==0 and counts['bum-pod-b']==1,counts)
 record('l2-bum-origin-only-export',bum)
 def reflection():
  token='KUBEOVN-OPR-REFLECT-1'
  caps=[capture(n,'blue','eth0','custom',token,'reflection-'+n) for n in ['node-a','node-b']]
  ex('node-b','ip','netns','exec','fabric','python3','/lab/source/hack/openperouter/packets.py','custom','eth0','02:00:00:00:01:11','ff:ff:ff:ff:ff:ff',token)
  counts=finish(caps);return require(all(c==0 for c in counts.values()),counts)
 record('l2-reflected-workload-source-drop',reflection)
 def reflection_nd():
  needle='fd00:101:1::11,ff02::1'
  caps=[capture(n,'blue','eth0','nd',needle,'reflection-nd-'+n) for n in ['node-a','node-b']]
  ex('node-b','ip','netns','exec','fabric','python3','/lab/source/hack/openperouter/packets.py','nd','eth0','02:00:00:00:01:11','33:33:00:00:00:01','fd00:101:1::11','ff02::1')
  counts=finish(caps);return require(all(c==0 for c in counts.values()),counts)
 record('l2-reflected-ipv6-nd-source-drop',reflection_nd)

 def external_bum():
  token='KUBEOVN-OPR-EXTERNAL-1'
  caps=[capture(n,'blue','eth0','custom',token,'external-bum-'+n) for n in ['node-a','node-b']]
  ex('node-b','ip','netns','exec','fabric','python3','/lab/source/hack/openperouter/packets.py','custom','eth0','02:aa:00:00:00:01','ff:ff:ff:ff:ff:ff',token)
  counts=finish(caps);return require(counts['external-bum-node-a']==0 and counts['external-bum-node-b']==1,counts)
 record('l2-external-bum-local-only',external_bum)
 def mtu():
  output=ex('node-a','ip','netns','exec','pods','ping','-n','-M','do','-s','1372','-c','2','-W','2','10.101.3.12')
  return require('0% packet loss' in output,output)
 record('internal-vxlan-mtu-1400',mtu)
 def mtu6():
  return ex('node-a','ip','netns','exec','pods','ping','-6','-n','-M','do','-s','1352','-c','2','-W','2','fd00:101:3::12')
 record('internal-vxlan-ipv6-mtu-1400',mtu6)
 def service():
  sh('node-b','mkdir -p /tmp/webroot; printf ok > /tmp/webroot/index.html; ip netns exec pods python3 -m http.server 8080 --bind 10.101.3.12 --directory /tmp/webroot >/tmp/http.log 2>&1 &')
  nb('lb-add','fixture-service','10.96.0.10:8080','10.101.3.12:8080','tcp')
  nb('ls-lb-add','pods','fixture-service');nb('lr-lb-add','tenant-a','fixture-service');nb('--wait=hv','sync')
  return ex('node-a','ip','netns','exec','pods','curl','-q','--noproxy','*','--max-time','5','-fsS','http://10.96.0.10:8080/')
 record('ovn-lb-service-vip-remote-backend',service)
 def deny_allow():
  match='inport == "pods-a" && ip4 && tcp.dst == 8080'
  nb('acl-add','pods','from-lport','2000',match,'drop');nb('--wait=hv','sync')
  sh('node-a',"if ip netns exec pods curl -q --noproxy '*' --max-time 3 -fsS http://10.96.0.10:8080/; then exit 1; fi")
  nb('acl-del','pods','from-lport','2000',match);nb('--wait=hv','sync')
  return ex('node-a','ip','netns','exec','pods','curl','-q','--noproxy','*','--max-time','5','-fsS','http://10.96.0.10:8080/')
 record('ovn-acl-deny-allow-service-vip',deny_allow)

 def restarted():
  sh('node-a','kill -TERM $(cat /tmp/ovn.pid); sleep 1; ovn-controller --detach --pidfile=/tmp/ovn.pid --unixctl=/tmp/ovn.ctl unix:/var/run/openvswitch/db.sock')
  wait(lambda: ex('node-a','ovs-appctl','-t','/tmp/ovn.ctl','connection-status')=='connected')
  return east_west()
 record('ovn-controller-restart-internal-connectivity',restarted)
except Exception as error:
 checks.append({'name':'fixture','status':'FAIL','error':str(error),'trace':traceback.format_exc()});(results/'ovn-dataplane-checks.json').write_text(json.dumps(checks,indent=2));print(traceback.format_exc(),flush=True)
finally:
 for node in created:
  (results/(node+'-docker.txt')).write_text(d('logs',names[node],check=False))
  for command,label in [(['ovs-vsctl','show'],'ovs'),(['ip','-brief','link'],'links')]:
   (results/(node+'-'+label+'.txt')).write_text(ex(node,*command,check=False))
 if 'db' in created:
  (results/'dataplane-nb.txt').write_text(nb('show'))
  (results/'dataplane-sb.txt').write_text(sb('show'))
 for node in reversed(created):d('rm','-f',names[node],check=False)
 if network_created:d('network','rm',network,check=False)
if not checks or any(c['status']!='PASS' for c in checks):raise SystemExit(1)
