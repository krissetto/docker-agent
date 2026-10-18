'use strict';
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');
const listeners = Object.create(null), watchFrames = [], requests = [], storage = Object.create(null);
global.localStorage = {getItem(k){return storage[k]||null;},setItem(k,v){storage[k]=String(v);}};
global.Pebble = {addEventListener(n,cb){listeners[n]=cb;},sendAppMessage(frame){watchFrames.push(JSON.parse(JSON.stringify(frame)));},openURL(url){this.url=url;}};
class XHR {
  constructor(){this.status=0;this.responseText='';this.headers={};}
  open(method,url){this.method=method;this.url=url;}
  setRequestHeader(k,v){this.headers[k]=v;}
  send(body){this.body=body;requests.push(this);}
  abort(){if(this.onabort)this.onabort();}
  respond(status,value){this.status=status;this.responseText=typeof value==='string'?value:JSON.stringify(value);if(this.onload)this.onload();}
}
global.XMLHttpRequest=XHR;
const phone=require('../src/pkjs/index.js');
function req(){const x=requests[requests.length-1];assert.ok(x,'expected a request');return x;}
function ack(start,requestID){
  let i=start;
  while(i<watchFrames.length){
    const f=watchFrames[i];
    if(f.request_id!==requestID)break;
    phone.command({payload:{type:9,request_id:requestID,frame:f.frame,index:f.index,receive_capacity:4096}});
    i++;
    if(f.frame===4)break;
  }
}
function framesFrom(start){return watchFrames.slice(start);}

// URL/auth policies, exact Pebble dictionary accounting and ES5-engine URL fallback.
assert.strictEqual(phone.normalizeRuntime({id:'h',base_url:'https://agent.example',auth_mode:'bearer'}).authMode,'bearer');
assert.throws(()=>phone.normalizeRuntime({base_url:'https://agent.example'}),/explicitly/);
assert.throws(()=>phone.normalizeRuntime({base_url:'https://agent.example',auth_mode:'private_no_auth'}),/confirmed private/);
assert.throws(()=>phone.normalizeRuntime({base_url:'https://public.example',auth_mode:'private_no_auth',trusted_network:true}),/private host/);
assert.strictEqual(phone.normalizeRuntime({base_url:'http://192.168.1.8',auth_mode:'private_no_auth',trusted_network:true,allow_insecure_local:true}).baseURL,'http://192.168.1.8');
assert.throws(()=>phone.normalizeRuntime({base_url:'http://192.168.1.8',auth_mode:'private_no_auth',trusted_network:true,allow_insecure_local:true,token:'secret'}),/Credentials/);
assert.strictEqual(phone.parseRuntimeURL('https://a.example:8443/path').origin,'https://a.example:8443');
assert.strictEqual(phone.parseRuntimeURL('https://a.example?token=x'),null);
assert.strictEqual(phone.preserveNumericMessageIDs('{"id":9223372036854775807,"text":"mentions \"id\":123"}'), '{"id":"9223372036854775807","text":"mentions \"id\":123"}');
assert.strictEqual(phone.tupleBytes('é'),10); // Pebble tuple header 7 + UTF8 2 + string NUL
assert.strictEqual(phone.dictionaryBytes({text:'x'}),10); // dictionary header 1 + tuple header 7 + payload 2
const urlSandbox={module:{exports:{}},exports:{},require:()=>'',URL:undefined,console,Math,Date,JSON,encodeURIComponent,decodeURIComponent,unescape,setTimeout:()=>1,clearTimeout(){},Pebble:undefined};
vm.runInNewContext(fs.readFileSync(require.resolve('../src/pkjs/index.js'),'utf8'),urlSandbox);
assert.strictEqual(urlSandbox.module.exports.normalizeRuntime({base_url:'http://10.1.2.3',auth_mode:'private_no_auth',trusted_network:true,allow_insecure_local:true}).authMode,'private_no_auth');
assert.strictEqual(urlSandbox.module.exports.parseRuntimeURL('https://user:pass@a.example'),null);

// No bearer data is persisted or bridged to the watch. Bearer HTTP is fail-closed.
let configReply;
phone.runtimeConfigMessage({data:{type:'runtime_config',selected_runtime_id:'home',alert_mode:1,runtimes:[{id:'home',name:'Home',base_url:'https://agent.example',auth_mode:'bearer',source:'workspace-source.yaml',root_agent:'planner',token:'PRIVATE'}]},respond(v){configReply=v;}});
assert.deepStrictEqual(configReply,{ok:true});
assert.ok(!Object.values(storage).some(v=>String(v).includes('PRIVATE')));
assert.ok(!watchFrames.some(f=>JSON.stringify(f).includes('PRIVATE')));
const rootsBefore=requests.length;
let deniedRoots;phone.listRootsConfigMessage({respond(v){deniedRoots=v;}},{runtime:{base_url:'https://agent.example',auth_mode:'bearer'},token:'PRIVATE'});
assert.strictEqual(deniedRoots.ok,false);assert.match(deniedRoots.error,/redirect protection/);assert.strictEqual(requests.length,rootsBefore);
phone.command({payload:{type:3,request_id:1,runtime_id:'home',receive_capacity:4096}});
assert.strictEqual(watchFrames[watchFrames.length-1].status,'error');
assert.strictEqual(watchFrames[watchFrames.length-1].request_id,1);

// Replace with explicit private No sign-in fixture; enumerate an authoritative source/root pair.
phone.runtimeConfigMessage({data:{type:'runtime_config',selected_runtime_id:'lan',alert_mode:1,runtimes:[{id:'lan',name:'Work LAN',base_url:'https://192.168.1.8',auth_mode:'private_no_auth',trusted_network:true,source:'workspace-source.yaml',root_agent:'planner'}]},respond(v){assert.strictEqual(v.ok,true);}});
let roots;
phone.listRootsConfigMessage({respond(v){roots=v;}},{runtime:{id:'lan',name:'Work LAN',base_url:'https://192.168.1.8',auth_mode:'private_no_auth',trusted_network:true},source:'workspace-source.yaml'});
req().respond(200,[{name:'workspace-source.yaml',root_agent_name:'planner',description:'Root'}]);
assert.deepStrictEqual(roots,{ok:true,limited:false,roots:[{source:'workspace-source.yaml',root_agent_name:'planner',description:'Root'}]});

// Watch IDs drive requests. Each response is begin/meta/rows/end, echoing request id.
let start=watchFrames.length;
phone.command({payload:{type:3,request_id:10,runtime_id:'lan',receive_capacity:4096}});
req().respond(200,[{name:'workspace-source.yaml',root_agent_name:'planner',description:'Root'}]);ack(start,10);
assert.deepStrictEqual(framesFrom(start).map(f=>f.status||f.frame),['agents',2,3,4]);
const sourceStart=watchFrames.length;
phone.command({payload:{type:3,request_id:11,agent_name:'planner',source:'workspace-source.yaml',receive_capacity:4096}});
assert.ok(req().url.includes('source=workspace-source.yaml'));
req().respond(200,{version:2,view:'summary',sessions:[{session_id:'root',source:'workspace-source.yaml',agent_name:'planner',loadable:true,title:'Root'}]});ack(sourceStart,11);
assert.deepStrictEqual(framesFrom(sourceStart).map(f=>f.status||f.frame),['sessions',2,3,4]);
const recent={version:2,session_id:'root',root_session_id:'root',source:'workspace-source.yaml',agent_name:'planner',runtime_status:'resident',state:'running',execution_available:true,pending:1,attention_known:true,approval_count:1,elicitation_count:0,before_cursor:'older-1',truncated_before:true,messages:[{id:'9223372036854775807',position:10,role:'assistant',agent_name:'planner',text:'hello 😀'}]};
const chatStart=watchFrames.length;
phone.command({payload:{type:4,request_id:12,session_id:'root',receive_capacity:4096}});req().respond(200,recent);ack(chatStart,12);
assert.deepStrictEqual(framesFrom(chatStart).map(f=>f.status||f.frame),['chat',2,3,4]);
assert.strictEqual(watchFrames[chatStart+1].agent_name,'planner');assert.strictEqual(watchFrames[chatStart+1].state,'running');assert.strictEqual(watchFrames[chatStart+2].row_id,'9223372036854775807');

// Stale cursor produces an explicit recoverable older-page message, not retry-loop/offline.
const staleStart=watchFrames.length;
phone.command({payload:{type:8,request_id:13,session_id:'root',before_cursor:'stale',receive_capacity:4096}});req().respond(409,{error:'conflict'});ack(staleStart,13);
assert.strictEqual(watchFrames[staleStart].status,'error');assert.match(watchFrames[staleStart+1].error,/cursor_stale/);assert.match(watchFrames[staleStart+1].text,/Older history expired/);

// Explicit start has one stable receipt ID across a lost watch ACK; key misuse is rejected.
const newStart=watchFrames.length;
phone.command({payload:{type:10,request_id:14,agent_name:'planner',source:'workspace-source.yaml',text:'hello',draft_key:'draft-a',receive_capacity:4096}});
const startBody=JSON.parse(req().body);assert.strictEqual(req().url,'https://192.168.1.8/api/v2/sessions/start');assert.strictEqual(startBody.input.request_id,'draft-a');
req().respond(202,{session_id:startBody.session_id,turn_id:'turn-1'});ack(newStart,14);
const accepted=watchFrames[newStart+1].session_id;assert.strictEqual(accepted,startBody.session_id);
const priorRequests=requests.length, retryStart=watchFrames.length;
phone.command({payload:{type:10,request_id:15,agent_name:'planner',source:'workspace-source.yaml',text:'hello',draft_key:'draft-a',receive_capacity:4096}});
assert.strictEqual(requests.length,priorRequests);assert.strictEqual(watchFrames[retryStart].status,'accepted');assert.strictEqual(phone.selected().session,accepted);ack(retryStart,15);
const conflictStart=watchFrames.length;
phone.command({payload:{type:10,request_id:16,agent_name:'planner',source:'workspace-source.yaml',text:'different',draft_key:'draft-a',receive_capacity:4096}});
assert.strictEqual(watchFrames[conflictStart].status,'error');ack(conflictStart,16);

// A read-only root never triggers a POST; /status is not called at all.
const ro={...recent,session_id:accepted,root_session_id:accepted,execution_available:false,messages:[]};
const readStart=watchFrames.length;
phone.command({payload:{type:4,request_id:17,session_id:accepted,receive_capacity:4096}});req().respond(200,ro);ack(readStart,17);
const postCount=requests.filter(x=>x.method==='POST').length;
phone.command({payload:{type:5,request_id:18,session_id:accepted,text:'reply',draft_key:'reply-key',receive_capacity:4096}});
req().respond(200,ro);
assert.strictEqual(requests.filter(x=>x.method==='POST').length,postCount);
assert.strictEqual(watchFrames[watchFrames.length-1].status,'error');

// Editing an existing runtime host/source invalidates its token and start receipts.
phone.reset();
phone.setRuntimeList([{id:'rotate',base_url:'https://one.example',auth_mode:'bearer'}]);
phone.runtimeConfigMessage({data:{type:'runtime_config',selected_runtime_id:'rotate',runtimes:[{id:'rotate',base_url:'https://one.example',auth_mode:'bearer',token:'not persisted'}],alert_mode:0},respond(v){assert(v.ok);}});
phone.setRuntimeList([{id:'rotate',base_url:'https://two.example',auth_mode:'bearer'}]);
assert.strictEqual(phone.selected().runtime.baseURL,'https://two.example');
console.log('Pebble phone integration tests passed');
