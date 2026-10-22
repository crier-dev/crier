#!/usr/bin/env node
// DF-CRIER-448: execute shipped render, navigation, composer and send functions.
// Fixture/DOM/HTTP are stubs, not a second implementation of the depth model.
// RED: git show c0d3142:cmd/server/chat.html > /tmp/df-crier-448-before.html
// node scripts/chat-thread-depth-truth.cjs --html /tmp/df-crier-448-before.html
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const args = process.argv.slice(2);
assert.ok(!args.length || (args.length === 2 && args[0] === '--html'), 'usage: chat-thread-depth-truth.cjs [--html <path>]');
let html = fs.readFileSync(args[1] || path.join(__dirname, '../cmd/server/chat.html'), 'utf8');
function extract(start, end) {
  const a = html.indexOf(start), b = html.indexOf(end, a);
  assert.ok(a >= 0 && b > a, `missing shipped region ${start}..${end}`);
  return html.slice(a, b);
}
function fixture() {
  const msg = (id, seq, thread, depth, parent = '', anchor = '') => ({id, seq, thread_id: thread, thread_depth: depth,
    parent_id: parent, anchor_message_id: anchor, payload: {text: id}, message_kind: 'plain', outcomes: []});
  return [msg('root',1,'root',0), msg('reply-1',2,'root',0,'root'), msg('reply-2',3,'root',0,'reply-1'),
    msg('b1',4,'b1',1,'','reply-2'), msg('b2',5,'b2',2,'','b1'), msg('b3',6,'b3',3,'','b2'),
    msg('deep-reply',7,'b3',3,'b3'), msg('b4',8,'b4',4,'','deep-reply'),
    msg('sibling',9,'sibling',3,'','b2'), msg('unrelated',10,'unrelated',0)];
}
function client() {
  const els = new Map(), requests = [];
  const $ = id => {
    if (!els.has(id)) els.set(id, {innerHTML:'',textContent:'',value:'',hidden:false,disabled:false,dataset:{},listeners:{},
      scrollHeight:100,scrollTop:0,clientHeight:100,addEventListener(type,fn){this.listeners[type]=fn;},
      setAttribute(){},removeAttribute(){},focus(){this.focused=true;},scrollIntoView(){this.scrolled=true;}});
    return els.get(id);
  };
  const state = {current:'room',session:{id:'room',state:'open'},messages:fixture(),systemEvents:[],thread:'',reply:null,
    agent:'atlas',namespace:'',sending:false,generation:0,participants:[]};
  const document = {getElementById: id => {
    // Only a genuinely rendered article can be the navigation target.
    if (!new RegExp('<article[^>]*id="'+id+'"').test($('transcript').innerHTML)) return null;
    return $(id);
  },createElement: () => {let text=''; return {set textContent(v){text=String(v);},get innerHTML(){return text.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}};}};
  const context = vm.createContext({$,state,document,console,Map,Set,Response,AbortSignal,DOMException,setTimeout,clearTimeout,
    crypto:require('node:crypto').webcrypto,author:()=> 'atlas',avatar:()=> 'A',stamp:String,messageText:m=>m.payload.text,
    resetAudienceWarning(){},composerReason:()=>'',updateComposer(){},openTaskThread:()=>false,taskMessageLink:()=>'',
    renderParticipants(){},refresh:async()=>{},clearSendProgress(){},startSendProgressWatcher(){},stopSendProgressWatcher(){},
    sessionPath:(id,suffix)=>`/sessions/${id}/${suffix}`,
    notice:(id,text,error=false)=>{$(id).textContent=text;$(id).className=error?'note error':'note';},
    fetch:async(route,options={})=>{requests.push({route,...options});return new Response(JSON.stringify({id:'new',seq:11,outcomes:[],targets:[]}),{status:201});}});
  vm.runInContext(extract('function esc(', 'function author(') + extract('async function call(', '// Management reads deliberately') +
    extract('function renderReply(', 'function loadedTaskLink(') + extract('let pendingMessageRequest', '$("add-participant-form").addEventListener') +
    extract('$("transcript").addEventListener("click"', '$("tasks").addEventListener("input"'), context);
  const run = s => vm.runInContext(s,context);
  const click = dataset => $('transcript').listeners.click({preventDefault(){},target:{closest:selector=>{
    const key = selector.match(/^\[data-([a-z-]+)\]$/)?.[1]?.replace(/-([a-z])/g,(_,c)=>c.toUpperCase());
    return key && dataset[key] !== undefined ? {dataset} : null;
  }}});
  $('text').value='new branch'; $('recipient').value='nimbus'; $('intent').value='message';
  return {$,state,run,click,requests,render:()=>run('renderMessages()'),ids:()=>[...$('transcript').innerHTML.matchAll(/<article[^>]*data-message-id="([^"]+)"/g)].map(m=>m[1])};
}
const cases = [
  ['named default is 3 and flat attribution chains all render at depth zero',()=>{
    const c=client(); c.render(); assert.equal(c.run('DEFAULT_THREAD_COLLAPSE_DEPTH'),3);
    for(const id of ['root','reply-1','reply-2']) assert.ok(c.ids().includes(id));
    assert.equal(c.state.messages[2].parent_id,'reply-1');
    assert.match(c.$('transcript').innerHTML,/data-anchor="reply-1"/);
    assert.match(c.$('transcript').innerHTML,/data-branch="reply-2"/);
  }],
  ['collapse card states exactly the hidden subtree count; sibling stays independent',()=>{
    const c=client(); c.render(); assert.deepEqual(c.ids(),['root','reply-1','reply-2','b1','b2','unrelated']);
    assert.match(c.$('transcript').innerHTML,/data-expand-branch="b3"[^>]*>[^<]*3 messages hidden/);
    assert.match(c.$('transcript').innerHTML,/data-expand-branch="sibling"[^>]*>[^<]*1 message hidden/);
    assert.doesNotMatch(c.$('transcript').innerHTML,/class="[^"]*(?:error|refused)/);
  }],
  ['expand reveals exactly its three hidden messages, not the sibling',()=>{
    const c=client();c.render();const before=c.ids();c.click({expandBranch:'b3'});
    assert.deepEqual(c.ids().filter(id=>!before.includes(id)),['b3','deep-reply','b4']);
    assert.ok(!c.ids().includes('sibling')); c.render(); assert.ok(c.ids().includes('b4'),'polling retains expansion');
  }],
  ['configurable depth is the renderer threshold, not reply hop length',()=>{
    const c=client();c.state.collapseDepth=2;c.render();assert.ok(!c.ids().includes('b2'));
    assert.ok(c.ids().includes('reply-2'));assert.match(c.$('transcript').innerHTML,/5 messages hidden/);
    c.state.collapseDepth=4;c.render();assert.ok(c.ids().includes('b3'));assert.ok(!c.ids().includes('b4'));
  }],
  ['parent action scrolls and focuses the exact reply, never its root',()=>{
    const c=client();c.render();c.click({anchor:'reply-1'});
    assert.equal(c.$('message-reply-1').focused,true);assert.equal(c.$('message-reply-1').scrolled,true);
    assert.notEqual(c.$('message-root').focused,true);assert.equal(c.state.locationMessage,'reply-1');
    assert.match(c.$('branch-rail').innerHTML,/aria-current="location"[^>]*>[^<]*#2/);
  }],
  ['cross-thread anchor exits filter and opens only the containing collapsed group',()=>{
    const c=client();c.state.thread='b4';c.render();c.click({anchor:'deep-reply'});
    assert.equal(c.state.thread,'');assert.ok(c.ids().includes('deep-reply'));assert.ok(!c.ids().includes('sibling'));
    assert.equal(c.$('message-deep-reply').focused,true);
  }],
  ['branch from a reply submits the exact parent and explicit branch flag',async()=>{
    const c=client();c.render();await c.click({branch:'reply-2'});assert.equal(c.state.reply,'reply-2');
    assert.match(c.$('replying').innerHTML,/Branch from/);await c.run('send({preventDefault(){}})');
    const posts=c.requests.filter(r=>r.method==='POST');assert.equal(posts.length,1);
    const body=JSON.parse(posts[0].body);assert.equal(body.parent_id,'reply-2');assert.equal(body.branch,true);
  }],
  ['ordinary Reply keeps the existing flat wire; cancel clears deliberate branching',async()=>{
    const c=client();c.click({branch:'b2'});c.click({reply:'reply-1'});assert.equal(c.state.branch,false);
    await c.run('send({preventDefault(){}})');const body=JSON.parse(c.requests.find(r=>r.method==='POST').body);
    assert.equal(body.parent_id,'reply-1');assert.equal(body.branch,undefined);
  }],
  ['renderer reads the named constant: mutating only its value changes default collapse',()=>{
    const original=html;
    try {
      html=html.replace('const DEFAULT_THREAD_COLLAPSE_DEPTH = 3;', 'const DEFAULT_THREAD_COLLAPSE_DEPTH = 2;');
      assert.notEqual(html,original,'named default must exist in shipped source');
      const c=client();c.render();assert.equal(c.run('threadCollapseDepth()'),2);
      assert.ok(c.ids().includes('b1'));assert.ok(!c.ids().includes('b2'));
    } finally { html=original; }
  }],
  ['system events remain visible and are never included in a hidden-message claim',()=>{
    const c=client();c.state.systemEvents=[{seq:7.5,text:'member joined'}];c.render();
    assert.match(c.$('transcript').innerHTML,/member joined/);
    assert.match(c.$('transcript').innerHTML,/3 messages hidden/);
    c.click({expandBranch:'b3'});assert.equal(c.ids().length,9);
  }],
  ['an in-flight branch action changes neither its anchor nor branch intent',()=>{
    const c=client();c.state.sending=true;c.click({branch:'reply-2'});
    assert.equal(c.state.reply,null);assert.notEqual(c.state.branch,true);assert.equal(c.requests.length,0);
  }],
  ['thread-filter counts only messages it actually hides',()=>{
    const c=client();c.state.thread='b3';c.render();assert.match(c.$('transcript').innerHTML,/2 messages hidden/);
    c.click({expandBranch:'b3'});assert.deepEqual(c.ids(),['b3','deep-reply']);
  }],
];
(async()=>{let failed=0;for(const [name,test] of cases){try{await test();console.log('PASS '+name);}catch(e){failed++;console.error('FAIL '+name+'\n  '+e.stack);}}
console.log(`RESULT ${cases.length-failed}/${cases.length} passed (stub DOM/HTTP, shipped functions)`);if(failed)process.exitCode=1;})();
