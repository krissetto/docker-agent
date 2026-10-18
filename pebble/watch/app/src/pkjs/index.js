'use strict';

var MAX_RUNTIMES = 8;
var MAX_RUNTIME_ROWS = 8;
var MAX_SESSIONS = 5;
var MAX_RUNTIME_ROWS = 8;
var MAX_AGENTS = 4;
var MAX_TURNS = 5;
var MAX_TEXT_BYTES = 192;
var MAX_OWNER_BYTES = 24;
var MAX_META_BYTES = 48;
var MAX_RECENT_CHAT_BYTES = 32 * 1024;
var MAX_CURSOR_BYTES = 192;
var MAX_ID_BYTES = 40;
var MAX_FRAME_BYTES = 4096;
var MAX_RESPONSE_BYTES = 36 * 1024;
var MIN_FRAME_BYTES = 1024;
var REQUEST_TIMEOUT_MS = 10000;
var FRAME_RETRIES_MS = [1000, 2000];
var RECEIPT_LIMIT = 8;
var RECEIPT_TTL_MS = 24 * 60 * 60 * 1000;
var CONFIG_KEY = 'docker-agent-pebble-runtime-config-v1';
var ALERT_KEY = 'docker-agent-pebble-alert-mode-v1';
var COMMAND = { hello:1, snapshot:2, select:3, openSession:4, send:5, discard:6, poll:7, older:8, ack:9, startNew:10 };
var FRAME = { begin:1, meta:2, row:3, end:4, ack:5, error:6 };
var ROW = { turn:0, session:1, agent:2, runtime:3 };
var FRAME_VIEW = { runtimes:0, agents:0, sessions:0, chat:0, sending:0, accepted:0, queued:0, error:0 };

var runtimes = [];
var selectedRuntime = null;
var selectedAgent = null;
var selectedSession = null;
var selectedView = 'runtimes';
var currentExecutionAvailable = false;
var bearerTokenByRuntime = Object.create(null);
var activeRequest = null;
var frameTimer = null;
var generation = 0;
var pushGeneration = 0;
var focused = true;
var baseline = true;
var runningBySession = Object.create(null);
var attentionCounts = Object.create(null);
var alertAtBySession = Object.create(null);
var attentionBySession = Object.create(null);
var alertAtBySession = Object.create(null);
var recentCursor = '';
var sendReceipts = [];
var outgoingFrames = null;
var outgoingIndex = 0;
var outgoingRetry = 0;
var receiveCapacity = MAX_FRAME_BYTES;
var alertMode = 0;

function utf8Bytes(value) { return unescape(encodeURIComponent(value)).length; }
function utf8Prefix(value, limit) {
  var output = '', used = 0;
  for (var i = 0; i < value.length;) {
    var codePoint = value.codePointAt(i), character = String.fromCodePoint(codePoint), size = utf8Bytes(character);
    if (used + size > limit) break;
    output += character; used += size; i += character.length;
  }
  return output;
}
function cleanText(value, limit) {
  var text = String(value || '').replace(/```[\s\S]*?```/g, '‹code›')
    .replace(/`([^`]+)`/g, '$1').replace(/\*\*(.*?)\*\*/g, '$1')
    .replace(/\*(.*?)\*/g, '$1').replace(/\[([^\]]+)\]\([^)]+\)/g, '$1')
    .replace(/[\r\t]+/g, ' ').replace(/\n{3,}/g, '\n\n').trim();
  return utf8Prefix(text, limit);
}
function parseRuntimeURL(value) {
  var text=String(value||'').trim();
  if(typeof URL!=='undefined') {
    try { var modern=new URL(text); if(modern.username||modern.password||modern.search||modern.hash)return null; return {protocol:modern.protocol,hostname:modern.hostname,username:modern.username,password:modern.password,search:modern.search,hash:modern.hash,origin:modern.origin,href:modern.href}; } catch (_) {}
  }
  var match=/^(https?):\/\/([^\/?#]+)([^?#]*)(\?[^#]*)?(#.*)?$/i.exec(text);
  if(!match)return null;
  var authority=match[2];
  if(authority.indexOf('@')>=0)return null;
  var hostname=authority,port='';
  if(authority.charAt(0)==='['){var close=authority.indexOf(']');if(close<0)return null;hostname=authority.slice(0,close+1);port=authority.slice(close+1);if(port&&!/^:\d{1,5}$/.test(port))return null;}
  else{var colon=authority.lastIndexOf(':');if(colon>=0){hostname=authority.slice(0,colon);port=authority.slice(colon);if(!/^:\d{1,5}$/.test(port))return null;}}
  if(!hostname||/[\s\\\\]/.test(hostname)||match[4]||match[5])return null;
  var scheme=match[1].toLowerCase()+'://';
  return {protocol:match[1].toLowerCase()+':',hostname:hostname.toLowerCase(),username:'',password:'',search:'',hash:'',origin:scheme+authority.toLowerCase(),href:scheme+authority.toLowerCase()+match[3]};
}
function isPrivateHost(host) {
  var value = host.toLowerCase().replace(/^\[|\]$/g, '');
  if (value === 'localhost' || value.endsWith('.local') || value === '::1') return true;
  if (value.indexOf(':') >= 0) return /^(fc|fd|fe80):/i.test(value);
  var match = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(value);
  if (!match) return false;
  var octets = match.slice(1).map(Number);
  if (octets.some(function (part) { return part > 255; })) return false;
  return octets[0] === 10 || octets[0] === 127 ||
    (octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31) ||
    (octets[0] === 192 && octets[1] === 168) || (octets[0] === 169 && octets[1] === 254);
}
function boundedID(value, limit) { var text=String(value||''); if(utf8Bytes(text)>limit) throw new Error('Runtime source or root agent ID exceeds its protocol bound'); if(/[\u0000-\u001f]/.test(text)) throw new Error('Runtime source or root agent ID contains a control character'); return text; }
function normalizeRuntime(input, index) {
  var url=parseRuntimeURL(input.base_url);
  if(!url||url.username||url.password||url.search||url.hash)throw new Error('Runtime URL is invalid or contains credentials, query or fragment');
  var mode = input.auth_mode;
  if (mode !== 'bearer' && mode !== 'private_no_auth') throw new Error('Choose Sign in or No sign-in explicitly');
  var trusted = input.trusted_network === true;
  if (input.token && mode !== 'bearer') throw new Error('Credentials cannot be used with No sign-in');
  if (mode === 'bearer' && url.protocol !== 'https:') throw new Error('Bearer sign-in requires HTTPS');
  if (mode === 'private_no_auth' && (!trusted || !isPrivateHost(url.hostname))) throw new Error('No sign-in requires a confirmed private host');
  if (url.protocol !== 'https:' && (url.protocol !== 'http:' || mode !== 'private_no_auth' || input.allow_insecure_local !== true)) {
    throw new Error('HTTPS is required except explicit private HTTP development opt-in');
  }
  return { id: String(input.id || 'runtime-' + index), name: cleanText(input.name || 'Runtime', 30),
    baseURL: url.href.replace(/\/$/, ''), source: boundedID(input.source || '', 40),
    rootAgent: boundedID(input.root_agent || '', 40), authMode: mode,
    trustedNetwork: trusted, allowInsecureLocal: !!input.allow_insecure_local };
}
function setRuntimeList(list) {
  if (!Array.isArray(list) || !list.length || list.length > MAX_RUNTIMES) throw new Error('Configure one to eight runtimes');
  var previous=Object.create(null),selectedID=selectedRuntime&&selectedRuntime.id;
  runtimes.forEach(function(runtime){previous[runtime.id]=runtime;});
  var ids=Object.create(null),normalized=list.map(normalizeRuntime);
  normalized.forEach(function(runtime){
    if(!runtime.id||ids[runtime.id])throw new Error('Runtime IDs must be unique');
    ids[runtime.id]=true;
    var old=previous[runtime.id];
    if(old&&(old.baseURL!==runtime.baseURL||old.source!==runtime.source||old.authMode!==runtime.authMode||old.trustedNetwork!==runtime.trustedNetwork)){
      delete bearerTokenByRuntime[runtime.id];
      sendReceipts=sendReceipts.filter(function(receipt){return receipt.runtime!==runtime.id;});
      if(selectedID===runtime.id){selectedAgent=selectedSession=null;baseline=true;}
    }
  });
  Object.keys(bearerTokenByRuntime).forEach(function(id){if(!ids[id])delete bearerTokenByRuntime[id];});
  sendReceipts=sendReceipts.filter(function(receipt){return !!ids[receipt.runtime];});
  runtimes=normalized;
  selectedRuntime=runtimes.find(function(runtime){return runtime.id===selectedID;})||runtimes[0];
}
function storeSafeRuntimes() {
  try {
    localStorage.setItem(CONFIG_KEY, JSON.stringify(runtimes.map(function (runtime) {
      return { id:runtime.id, name:runtime.name, base_url:runtime.baseURL, source:runtime.source,
        root_agent:runtime.rootAgent, auth_mode:runtime.authMode, trusted_network:runtime.trustedNetwork,
        allow_insecure_local:runtime.allowInsecureLocal };
    })));
    localStorage.setItem(ALERT_KEY, String(alertMode));
  } catch (_) {}
}
function restorePhoneState() {
  try {
    var saved = JSON.parse(localStorage.getItem(CONFIG_KEY) || '[]');
    if (Array.isArray(saved) && saved.length) setRuntimeList(saved);
    var mode = Number(localStorage.getItem(ALERT_KEY));
    if (mode === 0 || mode === 1 || mode === 2) alertMode = mode;
  } catch (_) { runtimes = []; }
  // Credentials are intentionally RAM-only; every PKJS launch starts signed out.
  bearerTokenByRuntime = Object.create(null);
}
var BEARER_REDIRECT_SAFE=false;
function tokenFor(runtime) { return runtime && runtime.authMode==='bearer' && BEARER_REDIRECT_SAFE ? (bearerTokenByRuntime[runtime.id] || '') : ''; }
function bearerTransportAvailable(runtime){return !runtime||runtime.authMode!=='bearer'||BEARER_REDIRECT_SAFE;}
function responseRequestID(requestID){return Number(requestID);}
function tupleBytes(value) { return 7 + (typeof value === 'string' ? utf8Bytes(value) + 1 : typeof value === 'boolean' ? 1 : 4); }
function dictionaryBytes(frame) { return Object.keys(frame).reduce(function (sum, key) { return sum + tupleBytes(frame[key]); }, 1); }
function cancelFrameTransfer() { if (frameTimer) clearTimeout(frameTimer); frameTimer=null; outgoingFrames=null;outgoingIndex=outgoingRetry=0; }
function sendFatalFrame(requestID, message) {
  cancelFrameTransfer();
  if (typeof Pebble.sendAppMessage !== 'function') return;
  var frame={type:COMMAND.snapshot,frame:FRAME.error,index:0,total:1,request_id:requestID,error:'bridge_unsupported'};
  if (message) frame.text=cleanText(message,MAX_META_BYTES);
  if (dictionaryBytes(frame) <= Math.min(receiveCapacity,MAX_FRAME_BYTES)) Pebble.sendAppMessage(frame,function(){},function(){});
}
function sendFrame(requestID) {
  if (!outgoingFrames) return;
  var frame = outgoingFrames[outgoingIndex];
  if (!frame) { cancelFrameTransfer(); return; }
  if (dictionaryBytes(frame) > Math.min(receiveCapacity, MAX_FRAME_BYTES)) {
    return sendFatalFrame(requestID, 'A bounded result row exceeds the negotiated phone link capacity.');
  }
  if (typeof Pebble.sendAppMessage !== 'function') { cancelFrameTransfer(); return; }
  Pebble.sendAppMessage(frame, function () {}, function () {});
  if (typeof Pebble.sendAppMessage !== 'function') return;
  if (frameTimer) clearTimeout(frameTimer);
  frameTimer = setTimeout(function retry() {
    if (!outgoingFrames) return;
    if (outgoingRetry >= FRAME_RETRIES_MS.length) {
      return sendFatalFrame(requestID, 'The watch did not acknowledge a bounded response.');
    }
    frameTimer = null;
    outgoingRetry++;
    Pebble.sendAppMessage(frame, function () {}, function () {});
    frameTimer = setTimeout(retry, FRAME_RETRIES_MS[outgoingRetry] || 2000);
  }, FRAME_RETRIES_MS[0]);
}
function sendFrameSet(requestID, view, metadata, rows) {
  requestID=responseRequestID(requestID); if(!Number.isFinite(requestID)) return;
  cancelFrameTransfer();
  var rowLimit=rows.length&&rows[0].kind===ROW.runtime?MAX_RUNTIMES:rows.length&&rows[0].kind===ROW.agent?MAX_AGENTS:rows.length&&rows[0].kind===ROW.session?MAX_SESSIONS:MAX_TURNS;
  if(rows.length>rowLimit)return sendFatalFrame(requestID,'View row count exceeds its protocol limit.');
  var total = rows.length + 3;
  var frames = [{type:COMMAND.snapshot,frame:FRAME.begin,index:0,total:total,request_id:requestID,status:view},
    {type:COMMAND.snapshot,frame:FRAME.meta,index:1,total:total,request_id:requestID}];
  if(metadata&&metadata.state!==undefined)frames[1].state=metadata.state;
  if(metadata&&metadata.agent_name!==undefined)frames[1].agent_name=metadata.agent_name;
  if(metadata&&metadata.session_id!==undefined)frames[1].session_id=metadata.session_id;
  Object.keys(metadata || {}).forEach(function(key){if(key!=='type'&&key!=='frame'&&key!=='index'&&key!=='total'&&key!=='request_id'){if(key==='selected_agent')frames[1].agent_name=metadata[key];else if(key==='selected_session')frames[1].session_id=metadata[key];else frames[1][key]=metadata[key];}});
  rows.forEach(function(row,index){frames.push({type:COMMAND.snapshot,frame:FRAME.row,index:index+2,total:total,request_id:requestID,
    row_index:index,row_kind:row.kind,row_id:String(row.id),row_role:cleanText(row.role||'',24),row_owner:cleanText(row.owner||'',MAX_OWNER_BYTES),
    row_text:cleanText(row.text||'',MAX_TEXT_BYTES),row_meta:row.kind===ROW.agent?boundedID(row.meta||'',40):cleanText(row.meta||'',MAX_META_BYTES),row_truncated:!!row.truncated});});
  frames.push({type:COMMAND.snapshot,frame:FRAME.end,index:total-1,total:total,request_id:requestID});
  outgoingFrames=frames;outgoingIndex=0;outgoingRetry=0;sendFrame(requestID);
}
function sendError(requestID,kind,message){if(!Number.isFinite(Number(requestID)))return;sendFrameSet(requestID,'error',{error:kind,text:cleanText(message||kind,MAX_META_BYTES)},[]);}
function abortRequest(){if(activeRequest){var old=activeRequest;activeRequest=null;old.abort();}}
function preserveNumericMessageIDs(json) {
  var output = '', i = 0;
  while (i < json.length) {
    if (json[i] !== '"') { output += json[i++]; continue; }
    var start = i++, escaped = false;
    while (i < json.length) {
      var ch = json[i++];
      if (escaped) { escaped = false; continue; }
      if (ch === '\\') { escaped = true; continue; }
      if (ch === '"') break;
    }
    var quoted = json.slice(start, i), key;
    try { key = JSON.parse(quoted); } catch (_) { output += quoted; continue; }
    output += quoted;
    if (key !== 'id') continue;
    var colon = i;
    while (colon < json.length && /\s/.test(json[colon])) colon++;
    if (json[colon] !== ':') continue;
    var numberStart = colon + 1;
    while (numberStart < json.length && /\s/.test(json[numberStart])) numberStart++;
    var match = /^-?\d+/.exec(json.slice(numberStart));
    if (!match) continue;
    var numberEnd = numberStart + match[0].length, boundary = numberEnd;
    while (boundary < json.length && /\s/.test(json[boundary])) boundary++;
    if (json[boundary] !== ',' && json[boundary] !== '}') continue;
    output += json.slice(i, numberStart) + '"' + match[0] + '"';
    i = numberEnd;
  }
  return output;
}
function request(method,path,body,callback,context){
  abortRequest();if(!selectedRuntime)return callback('setup');
  var xhr=new XMLHttpRequest(),requestGeneration=generation,tooLarge=false,requestedURL=selectedRuntime.baseURL;activeRequest=xhr;
  var timeout=setTimeout(function(){xhr.abort();},REQUEST_TIMEOUT_MS);
  xhr.open(method,selectedRuntime.baseURL+path,true);xhr.timeout=REQUEST_TIMEOUT_MS;xhr.setRequestHeader('Accept','application/json');
  if(body)xhr.setRequestHeader('Content-Type','application/json');var token=tokenFor(selectedRuntime);if(token)xhr.setRequestHeader('Authorization','Bearer '+token);
  xhr.onprogress=function(event){if(event.loaded>MAX_RESPONSE_BYTES){tooLarge=true;xhr.abort();}};
  xhr.onload=function(){clearTimeout(timeout);if(activeRequest===xhr)activeRequest=null;if(requestGeneration!==generation)return;
    if(tooLarge||utf8Bytes(xhr.responseText||'')>MAX_RESPONSE_BYTES)return callback('unsupported');
    if(xhr.responseURL){var finalURL=parseRuntimeURL(xhr.responseURL),initialURL=parseRuntimeURL(requestedURL);if(!finalURL||!initialURL||finalURL.origin!==initialURL.origin)return callback('unsupported');}
    if(xhr.status===401||xhr.status===403)return callback('unauthorized');if(xhr.status===409&&path.indexOf('/recent-chat')>=0)return callback('cursor_stale');if(xhr.status<200||xhr.status>=300)return callback(xhr.status===404?'unsupported':'offline');
    var value;try{var raw=preserveNumericMessageIDs(xhr.responseText);value=JSON.parse(raw);}catch(_){return callback('unsupported');}
    callback(null,value,context);
  };
  xhr.onerror=xhr.ontimeout=xhr.onabort=function(){clearTimeout(timeout);if(activeRequest===xhr)activeRequest=null;if(requestGeneration===generation)callback(tooLarge?'unsupported':'offline');};
  xhr.send(body?JSON.stringify(body):null);return xhr;
}

function runtimeRows(){return runtimes.slice(0,MAX_RUNTIME_ROWS).map(function(runtime){var mode=runtime.authMode;var state=mode==='bearer'&&!tokenFor(runtime)?'Sign in on phone':mode==='private_no_auth'?'Trusted network':'Signed in for this app run';return{kind:ROW.runtime,id:runtime.id,role:mode,owner:'',text:runtime.name,meta:state};});}
function sendRuntimes(requestID){selectedView='runtimes';sendFrameSet(requestID,'runtimes',{runtime_id:selectedRuntime?selectedRuntime.id:'',runtime_name:selectedRuntime?selectedRuntime.name:'',source:selectedRuntime?selectedRuntime.source:'',alert_mode:alertMode},runtimeRows());}
function loadAgents(requestID){
  if(!selectedRuntime)return sendRuntimes(requestID);
  if(selectedRuntime.authMode==='bearer'&&!bearerTransportAvailable(selectedRuntime))return sendError(requestID,'bearer_unsupported','Bearer API transport redirect protection is not verified in this Pebble runtime. Use an explicit trusted private no-sign-in runtime or a newer verified companion transport.');
  if(selectedRuntime.authMode==='bearer'&&!tokenFor(selectedRuntime))return sendError(requestID,'needs_auth','Sign in on phone; bearer credentials are not persisted.');
  request('GET','/api/agents',null,function(error,sources){
    if(error)return sendError(requestID,error,error);
    if(!Array.isArray(sources))return sendError(requestID,'unsupported','Unsupported source list.');
    var candidates=sources.filter(function(row){return row&&typeof row.name==='string'&&typeof row.root_agent_name==='string'&&row.root_agent_name;});
    var matching=selectedRuntime.source||selectedRuntime.rootAgent?candidates.filter(function(row){return (!selectedRuntime.source||row.name===selectedRuntime.source)&&(!selectedRuntime.rootAgent||row.root_agent_name===selectedRuntime.rootAgent);}):candidates;
    if(!matching.length)return sendError(requestID,'agent_missing','Configured source/root agent is unavailable; reselect on phone.');
    if(!candidates.length)return sendError(requestID,'unsupported','Server does not advertise the exact root_agent_name source contract.');
    if(!selectedRuntime.source||!selectedRuntime.rootAgent)return sendError(requestID,'source_required','Choose an exact source/root pair in phone settings before selecting an agent.');
    if(matching.length>MAX_AGENTS)return sendError(requestID,'source_limit','Pinned source returned too many root-agent rows.');
    selectedView='agents';
    sendFrameSet(requestID,'agents',{runtime_id:selectedRuntime.id,runtime_name:selectedRuntime.name,source:selectedRuntime.source,agent_name:selectedAgent||'',agent_count:matching.length},matching.map(function(row){
      var source=boundedID(row.name,40),root=boundedID(row.root_agent_name,40);
      return {kind:ROW.agent,id:root,role:'root',owner:'',text:root,meta:source};
    }));
  });
}

function loadSessions(requestID){
  if(!selectedRuntime||!selectedAgent||!selectedRuntime.source)return sendError(requestID,'agent_missing','Choose an exact source and root agent.');
  request('GET','/api/v2/sessions?view=summary&include_children=false&limit=10&source='+encodeURIComponent(selectedRuntime.source),null,function(error,catalog){
    if(error)return sendError(requestID,error,error);if(!catalog||catalog.version!==2||catalog.view!=='summary'||!Array.isArray(catalog.sessions))return sendError(requestID,'unsupported','Session summary view is unsupported.');
    var rows=catalog.sessions.filter(function(row){return row&&!row.parent_id&&row.source===selectedRuntime.source&&row.agent_name===selectedAgent&&row.loadable&&!row.route_error;}).slice(0,MAX_SESSIONS);
    selectedView='sessions';sendFrameSet(requestID,'sessions',{runtime_id:selectedRuntime.id,runtime_name:selectedRuntime.name,source:selectedRuntime.source,agent_name:selectedAgent,session_count:rows.length,has_more:catalog.next_cursor?1:0},rows.map(function(row){return{kind:ROW.session,id:String(row.session_id),role:row.state_known?row.state:'unknown',owner:selectedAgent,text:row.title||'Conversation',meta:'',truncated:false};}));
  });
}
function recentPath(id,cursor){var path='/api/v2/sessions/'+encodeURIComponent(id)+'/recent-chat?limit='+MAX_TURNS;if(selectedRuntime.source)path+='&source='+encodeURIComponent(selectedRuntime.source);if(cursor)path+='&cursor='+encodeURIComponent(cursor);return path;}
function recentForSelected(requestID,callback,cursor){
  if(!selectedRuntime||!selectedAgent||!selectedSession)return sendError(requestID,'unsupported','No selected root session.');
  request('GET',recentPath(selectedSession,cursor),null,function(error,recent){if(error==='cursor_stale')return sendError(requestID,'cursor_stale','Older history expired · retry from newest chat.');
    if(error)return sendError(requestID,error,error);
    if(!recent||recent.version!==2||recent.session_id!==selectedSession||recent.root_session_id!==selectedSession||recent.agent_name!==selectedAgent||!Array.isArray(recent.messages))return sendError(requestID,'root_mismatch','Selected session is not this exact root agent.');
    callback(recent);
  });
}
function runningState(recent){return['queued','running','cancelling'].indexOf(recent.state)>=0;}
function turnRows(recent){return recent.messages.slice(-MAX_TURNS).map(function(message){return{kind:ROW.turn,id:String(message.id),role:message.role,owner:message.agent_name||'',text:cleanText(message.text||'',MAX_TEXT_BYTES),meta:'',truncated:!!message.truncated};});}
function renderRecent(recent,requestID){
  currentExecutionAvailable=!!recent.execution_available;
  var rows=turnRows(recent),key=selectedRuntime.id+'\0'+selectedSession;
  var prior=attentionCounts[key]||{approval:0,elicitation:0},known=!!recent.attention_known,alert=0;
  if(!baseline&&known&&(Number(recent.approval_count)>prior.approval||Number(recent.elicitation_count)>prior.elicitation))alert=1;
  if(known)attentionCounts[key]={approval:Number(recent.approval_count)||0,elicitation:Number(recent.elicitation_count)||0};else delete attentionCounts[key];
  var running=runningState(recent);
  if(!baseline&&runningBySession[key]&&!running&&Date.now()-(alertAtBySession[key]||0)>=30000){if(!alert)alert=2;alertAtBySession[key]=Date.now();}
  runningBySession[key]=running;baseline=false;recentCursor=recent.before_cursor||'';selectedView='chat';
  var metadata={runtime_id:selectedRuntime.id,runtime_name:selectedRuntime.name,source:recent.source||'',agent_name:recent.agent_name,session_id:recent.session_id,root_session_id:recent.root_session_id,
    session_title:'',runtime_status:recent.runtime_status,state:recent.state||'unknown',execution_available:!!recent.execution_available,pending:Number(recent.pending)||0,
    attention_known:known,approval_count:Number(recent.approval_count)||0,elicitation_count:Number(recent.elicitation_count)||0,alert:alert,alert_mode:alertMode,
    truncated_before:!!recent.truncated_before,before_cursor:recent.before_cursor||''};
  sendFrameSet(requestID,'chat',metadata,rows);
}
function openRecent(requestID){recentForSelected(requestID,function(recent){renderRecent(recent,requestID);});}
function receiptHash(text){var h=2166136261,h2=0x9e3779b9;for(var i=0;i<text.length;i++){h=Math.imul(h^text.charCodeAt(i),16777619);h2=Math.imul(h2^text.charCodeAt(i),2246822519);}return(h>>>0).toString(16)+(h2>>>0).toString(16);}
function makeSessionID(runtime,agent,draftKey){return'watch-'+draftKey;}
function findReceipt(runtime,agent,draftKey,text){var now=Date.now();sendReceipts=sendReceipts.filter(function(entry){return now-entry.at<RECEIPT_TTL_MS;}).slice(-RECEIPT_LIMIT);var found=sendReceipts.find(function(entry){return entry.runtime===runtime.id&&entry.agent===agent&&entry.draftKey===draftKey;});if(found&&(found.text!==text||found.source!==runtime.source||found.baseURL!==runtime.baseURL))throw new Error('Draft key was already used for another target or text.');return found;}
function saveReceipt(receipt){var index=sendReceipts.findIndex(function(entry){return entry.runtime===receipt.runtime&&entry.agent===receipt.agent&&entry.draftKey===receipt.draftKey;});if(index>=0)sendReceipts.splice(index,1);sendReceipts.push(receipt);sendReceipts=sendReceipts.slice(-RECEIPT_LIMIT);}
function sendNewDraft(data,requestID){
  var text=typeof data.text==='string'?data.text:'',draftKey=typeof data.draft_key==='string'?data.draft_key:'';
  if(!selectedRuntime||!selectedAgent||!selectedRuntime.source||!text||!draftKey)return sendError(requestID,'unsupported','Draft requires selected source, root agent, and stable request key.');
  var receipt;try{receipt=findReceipt(selectedRuntime,selectedAgent,draftKey,text);}catch(error){return sendError(requestID,'conflict',error.message);}
  if(receipt&&receipt.result){selectedSession=receipt.sessionID;return sendFrameSet(requestID,receipt.result.disposition==='queued'?'queued':'accepted',receipt.result,[]);}
  if(!receipt){receipt={runtime:selectedRuntime.id,baseURL:selectedRuntime.baseURL,agent:selectedAgent,source:selectedRuntime.source,draftKey:draftKey,text:text,sessionID:makeSessionID(selectedRuntime,selectedAgent,draftKey),at:Date.now(),result:null};saveReceipt(receipt);}
  request('POST','/api/v2/sessions/start',{agent_name:selectedAgent,source:selectedRuntime.source||'',session_id:receipt.sessionID,input:{content:text,request_id:draftKey}},function(error,result){
    if(error)return sendError(requestID,error,error);
    if(!result||result.session_id!==receipt.sessionID||!result.turn_id)return sendError(requestID,'unsupported','Server did not confirm generated root ID.');
    receipt.result={session_id:result.session_id,turn_id:result.turn_id,disposition:result.disposition||'accepted'};receipt.at=Date.now();saveReceipt(receipt);selectedSession=result.session_id;
    sendFrameSet(requestID,receipt.result.disposition==='queued'?'queued':'accepted',{session_id:result.session_id,selected_session:result.session_id,agent_name:selectedAgent,selected_agent:selectedAgent,disposition:receipt.result.disposition,text:result.turn_id},[]);
  });
}
function sendExistingDraft(data,requestID){
  var idempotency=String(data.draft_key||'');
  if(!selectedRuntime||!selectedAgent||!data.session_id||data.session_id!==selectedSession||!data.text||!idempotency)return sendError(requestID,'unsupported','Reply target is not selected.');
  recentForSelected(requestID,function(recent){
    if(recent.root_session_id!==data.session_id||!recent.execution_available)return sendError(requestID,'history_only','History-only sessions cannot execute replies.');
    request('POST','/api/v2/sessions/'+encodeURIComponent(data.session_id)+'/messages',{content:String(data.text),request_id:idempotency},function(error,result){
      if(error)return sendError(requestID,error,error);if(!result||result.session_id!==data.session_id||!result.turn_id)return sendError(requestID,'unsupported','Server did not confirm reply submission.');
      sendFrameSet(requestID,result.disposition==='queued'?'queued':'accepted',{session_id:result.session_id,selected_session:result.session_id,agent_name:selectedAgent,selected_agent:selectedAgent,disposition:result.disposition||'accepted',text:result.turn_id},[]);
    });
  });
}
function poll(requestID){if(!selectedRuntime)return sendRuntimes(requestID);if(!selectedAgent)return loadAgents(requestID);if(!selectedSession)return loadSessions(requestID);openRecent(requestID);}
function command(event){var data=event&&event.payload||{},requestID=Number(data.request_id),capacity=Number(data.receive_capacity);if(!Number.isFinite(requestID))return;
  if(capacity){receiveCapacity=Math.min(MAX_FRAME_BYTES,capacity);if(receiveCapacity<MIN_FRAME_BYTES)return sendError(requestID,'bridge_unsupported','Minimum watch AppMessage inbox is 1024 bytes.');}
  var type=Number(data.type);
  if(type===COMMAND.ack){var frame=outgoingFrames&&outgoingFrames[outgoingIndex];if(!frame||data.request_id!==frame.request_id)return;if(data.index===frame.index-1)return;if(data.index!==frame.index||data.frame!==frame.frame)return;if(frameTimer)clearTimeout(frameTimer);frameTimer=null;outgoingRetry=0;outgoingIndex++;if(outgoingIndex===outgoingFrames.length){cancelFrameTransfer();return;}return sendFrame(requestID);}
  switch(type){
    case COMMAND.hello:selectedView='runtimes';return sendRuntimes(requestID);
    case COMMAND.select:
      if(data.runtime_id){selectedRuntime=runtimes.find(function(runtime){return runtime.id===String(data.runtime_id);})||null;selectedAgent=selectedSession=null;if(!selectedRuntime)return sendError(requestID,'unsupported','Unknown runtime.');return loadAgents(requestID);}
      if(data.agent_name){if(!selectedRuntime||!data.source||!runtimes.some(function(runtime){return runtime.id===selectedRuntime.id&&(!runtime.source||runtime.source===data.source)&&(!runtime.rootAgent||runtime.rootAgent===data.agent_name);}))return sendError(requestID,'agent_missing','Select the exact source and root agent.');selectedRuntime.source=String(data.source);selectedRuntime.rootAgent=String(data.agent_name);selectedAgent=String(data.agent_name);selectedSession=null;storeSafeRuntimes();return loadSessions(requestID);}
      return sendError(requestID,'unsupported','Select runtime or agent explicitly.');
    case COMMAND.openSession:selectedSession=String(data.session_id||'');baseline=true;return openRecent(requestID);
    case COMMAND.send:return sendExistingDraft(data,requestID);
    case COMMAND.discard: return sendFrameSet(requestID,'chat',{status:'draft_discarded'},[]);
    case COMMAND.poll:return poll(requestID);
    case COMMAND.older:if(!data.before_cursor||!selectedSession)return sendError(requestID,'unsupported','Older cursor missing.');return recentForSelected(requestID,function(recent){renderRecent(recent,requestID);},String(data.before_cursor));
    case COMMAND.startNew:return sendNewDraft(data,requestID);
    default:return sendError(requestID,'unsupported','Unknown command.');
  }
}
function setAlertMode(mode){var value=Number(mode);if(value!==0&&value!==1&&value!==2)throw new Error('Alert mode must be haptic, sound+haptic, or silent');alertMode=value;try{localStorage.setItem(ALERT_KEY,String(value));}catch(_){}}
function testRuntime(input,token,callback){var runtime;try{runtime=normalizeRuntime(input,0);}catch(error){return callback(error.message);}requestRuntimeTest(runtime,token,function(error,info){if(error)return callback(error);callback(null,info);});}
function requestRuntimeTest(runtime,token,callback){
  var xhr=new XMLHttpRequest(),tooLarge=false;xhr.open('GET',runtime.baseURL+'/api/v2/server',true);xhr.timeout=REQUEST_TIMEOUT_MS;xhr.setRequestHeader('Accept','application/json');
  if(runtime.authMode==='bearer')return callback('Bearer API transport redirect protection is not verified.');
  xhr.onprogress=function(event){if(event.loaded>4096){tooLarge=true;xhr.abort();}};
  xhr.onload=function(){if(tooLarge||utf8Bytes(xhr.responseText||'')>4096)return callback('Runtime identity response too large');if(xhr.responseURL){try{if(new URL(xhr.responseURL).origin!==new URL(runtime.baseURL).origin)return callback('Cross-origin redirect blocked');}catch(_){return callback('Invalid final runtime URL');}}if(xhr.status===401||xhr.status===403)return callback('Token rejected');if(xhr.status<200||xhr.status>=300)return callback('Runtime unavailable');try{var info=JSON.parse(xhr.responseText);if(info.session_api_version!==2)return callback('docker-agent session API v2 is required');callback(null,info);}catch(_){callback('Invalid docker-agent response');}};
  xhr.onerror=xhr.ontimeout=xhr.onabort=function(){callback(tooLarge?'Runtime identity response too large':'Connection failed');};xhr.send();
}

function testConfigMessage(event){var msg=event.data||{},token=String(msg.token||'');try{var runtime=normalizeRuntime(msg.runtime,0);if(runtime.authMode==='bearer'&&!token)throw new Error('Bearer token required');requestRuntimeTest(runtime,token,function(error,info){event.respond(error?{ok:false,error:error}:{ok:true,name:cleanText(runtime.name,30),session_api_version:2});});}catch(error){event.respond({ok:false,error:cleanText(error.message,MAX_META_BYTES)});}}
function listRootsConfigMessage(event,msg){
  var token=typeof msg.token==='string'?msg.token:'';
  var runtime;
  try{runtime=normalizeRuntime(msg.runtime,0);if(runtime.authMode==='bearer'&&!token)throw new Error('Bearer token required');}
  catch(error){return event.respond({ok:false,error:cleanText(error.message,MAX_META_BYTES)});}
  var xhr=new XMLHttpRequest();xhr.open('GET',runtime.baseURL+'/api/agents',true);xhr.timeout=REQUEST_TIMEOUT_MS;xhr.setRequestHeader('Accept','application/json');
  if(runtime.authMode==='bearer')return event.respond({ok:false,error:'Bearer API transport redirect protection is not verified in this Pebble runtime.'});
  var tooLarge=false;
  xhr.onprogress=function(eventProgress){if(eventProgress.loaded>8192){tooLarge=true;xhr.abort();}};
  xhr.onload=function(){
    if(tooLarge)return event.respond({ok:false,error:'Root source list exceeds the bounded settings response'});
    if(xhr.responseURL){try{if(new URL(xhr.responseURL).origin!==new URL(runtime.baseURL).origin)return event.respond({ok:false,error:'Cross-origin redirect blocked'});}catch(_){return event.respond({ok:false,error:'Invalid final runtime URL'});}}
    if(utf8Bytes(xhr.responseText||'')>8192)return event.respond({ok:false,error:'Root source list exceeds the bounded settings response'});
    if(xhr.status===401||xhr.status===403)return event.respond({ok:false,error:'Token rejected'});
    if(xhr.status<200||xhr.status>=300)return event.respond({ok:false,error:'Root source list unavailable'});
    var roots;try{roots=JSON.parse(xhr.responseText);}catch(_){return event.respond({ok:false,error:'Invalid source list'});}
    if(!Array.isArray(roots))return event.respond({ok:false,error:'Invalid source list'});
    var mapped=[];var sawUnsupported=false;
    roots.forEach(function(row){if(!row||typeof row.name!=='string')return;if(typeof row.root_agent_name!=='string'||!row.root_agent_name){sawUnsupported=true;return;}
      try{mapped.push({source:boundedID(row.name,40),root_agent_name:boundedID(row.root_agent_name,40),description:cleanText(row.description||'',48)});}catch(_){sawUnsupported=true;}
    });
    if(sawUnsupported)return event.respond({ok:false,error:'This server does not advertise bounded root_agent_name source metadata'});
    var requested=typeof msg.source==='string'?msg.source:'';
    if(requested){try{requested=boundedID(requested,40);}catch(error){return event.respond({ok:false,error:error.message});}mapped=mapped.filter(function(row){return row.source===requested;});}
    var limited=mapped.length>8||roots.length>8;
    event.respond({ok:true,limited:limited,roots:mapped.slice(0,8)});
  };
  xhr.onerror=xhr.ontimeout=xhr.onabort=function(){event.respond({ok:false,error:tooLarge?'Root source list exceeds the bounded settings response':'Connection failed'});};xhr.send();
}

function runtimeConfigMessage(event){if(!event||typeof event.respond!=='function'||!event.data||typeof event.data!=='object')return;var msg=event.data;
  if(msg.type==='test_runtime')return testConfigMessage(event);
  if(msg.type==='list_roots')return listRootsConfigMessage(event,msg);
  if(msg.type==='get_config'){return event.respond({ok:true,runtimes:runtimes.map(function(runtime){return{id:runtime.id,name:runtime.name,base_url:runtime.baseURL,source:runtime.source,root_agent:runtime.rootAgent,auth_mode:runtime.authMode,trusted_network:runtime.trustedNetwork,allow_insecure_local:runtime.allowInsecureLocal};}),selected_runtime_id:selectedRuntime&&selectedRuntime.id,alert_mode:alertMode});}
  if(msg.type!=='runtime_config'||!Array.isArray(msg.runtimes))return event.respond({ok:false,error:'invalid'});
  try{setRuntimeList(msg.runtimes);generation++;abortRequest();cancelFrameTransfer();bearerTokenByRuntime=Object.create(null);msg.runtimes.forEach(function(input,index){var runtime=runtimes[index];if(runtime.authMode==='bearer'){if(typeof input.token!=='string'||!input.token||input.token.length>4096)throw new Error('Bearer token required');bearerTokenByRuntime[runtime.id]=input.token;}});setAlertMode(msg.alert_mode===undefined?0:msg.alert_mode);storeSafeRuntimes();selectedRuntime=runtimes.find(function(runtime){return runtime.id===msg.selected_runtime_id;})||runtimes[0];selectedAgent=selectedSession=null;baseline=true;event.respond({ok:true});}catch(error){event.respond({ok:false,error:cleanText(error.message,MAX_META_BYTES)});}}
function acceptLegacyNonSecretResponse(response){if(!response)return;try{var config=JSON.parse(decodeURIComponent(response));if(config.type!=='runtime_config'||!Array.isArray(config.runtimes)||config.runtimes.some(function(item){return item.token||item.auth_token||item.auth_mode==='bearer';}))return;setRuntimeList(config.runtimes);setAlertMode(config.alert_mode||0);storeSafeRuntimes();selectedRuntime=runtimes[0];}catch(_){}}

function privateConfig(){try{var saved=JSON.parse(localStorage.getItem(CONFIG_KEY)||'[]');if(Array.isArray(saved)&&saved.length)setRuntimeList(saved);var mode=Number(localStorage.getItem(ALERT_KEY));if(mode===0||mode===1||mode===2)alertMode=mode;}catch(_){runtimes=[];}bearerTokenByRuntime=Object.create(null);}

var CONFIG_HTML = require('./config-page.js');

function openSettings() {
  if (typeof Pebble === 'undefined' || typeof Pebble.openURL !== 'function') return false;
  Pebble.openURL('data:text/html;charset=utf-8,' + encodeURIComponent(CONFIG_HTML));
  return true;
}

if (typeof Pebble !== 'undefined' && typeof Pebble.addEventListener === 'function') {
  Pebble.addEventListener('ready', function () { restorePhoneState(); });
  Pebble.addEventListener('appmessage', function (event) { command(event); });
  Pebble.addEventListener('configmessage', runtimeConfigMessage);
  Pebble.addEventListener('webviewclosed', function (event) { acceptLegacyNonSecretResponse(event && event.response); });
  Pebble.addEventListener('showConfiguration', function () {
    openSettings();
  });
  Pebble.addEventListener('appfocus', function (event) {
    focused = !!(event && event.payload);
    if (!focused) {
      generation++;
      abortRequest(); cancelFrameTransfer(); bearerTokenByRuntime=Object.create(null);
      return;
    }
    baseline=true; restorePhoneState();
  });
}

var AgentPhone = {
  normalizeRuntime:normalizeRuntime,
  parseRuntimeURL:parseRuntimeURL,
  cleanText:cleanText,
  preserveNumericMessageIDs:preserveNumericMessageIDs,
  tupleBytes:tupleBytes,
  dictionaryBytes:dictionaryBytes,
  setRuntimeList:setRuntimeList,
  handleAppMessage:command,
  command:command,
  runtimeConfigMessage:runtimeConfigMessage,
  listRootsConfigMessage:listRootsConfigMessage,
  acceptLegacyNonSecretResponse:acceptLegacyNonSecretResponse,
  request:request,
  sendFrameSet:sendFrameSet,
  sendNewDraft:sendNewDraft,
  sendExistingDraft:sendExistingDraft,
  renderRecent:renderRecent,
  projectRecent:function(value,sessionID,agentName){
    if(!value||value.version!==2||value.session_id!==sessionID||value.root_session_id!==sessionID||value.agent_name!==agentName||!Array.isArray(value.messages))return null;
    return{status:value.state||'unknown',runtimeStatus:value.runtime_status||'unknown',executionAvailable:!!value.execution_available,attentionKnown:!!value.attention_known,
      approvalCount:Number(value.approval_count)||0,elicitationCount:Number(value.elicitation_count)||0,pending:Number(value.pending)||0,
      beforeCursor:value.before_cursor||'',truncatedBefore:!!value.truncated_before,rows:turnRows(value)};
  },
  setAlertMode:setAlertMode,
  selected:function(){return{runtime:selectedRuntime,agent:selectedAgent,session:selectedSession,executionAvailable:currentExecutionAvailable,view:selectedView,alertMode:alertMode};},
  reset:function(){abortRequest();cancelFrameTransfer();bearerTokenByRuntime=Object.create(null);runtimes=[];selectedRuntime=selectedAgent=selectedSession=null;baseline=true;}
};
if(typeof module!=='undefined')module.exports=AgentPhone;
