import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createHost } from './host.mjs';

const op = {type:'approval', token:'turn:confirm:1', id:'call-1', name:'add_memory', args:'{"memory":"do not store without approval"}'};
const signal = () => new AbortController().signal;

test('no callback refuses confirmation even with autoApprove', async () => {
  const host = createHost({autoApprove:true}, undefined, false);
  try {
    assert.deepEqual(await host.approval(op, signal()), {approved:false, reason:''});
    assert.throws(() => host.confirm(true), /no tool confirmation/);
  } finally { await host.close(); }
});

test('binds an approval to the current tool; rejects stale callbacks and wrong tool names', async () => {
  const controller = new AbortController();
  let event;
  const host = createHost({}, e => { event=e; }, false);
  const turn = host.approval(op, controller.signal);
  assert.deepEqual(event, {type:'tool_confirmation',confirmationId:op.token,id:'call-1',name:op.name,args:op.args});
  assert.throws(() => host.confirm({decision:'approve_tool',confirmationId:op.token,toolName:'delete_plan'}), /matching pending/);
  controller.abort();
  await assert.rejects(turn, /aborted/);
  assert.throws(() => host.confirm(true), /no tool confirmation/);
  await host.close();
});

test('stateless approval callbacks can decline asynchronously with a reason', async () => {
  const events=[];
  const host = createHost({}, async e => { events.push(e.type); return {decision:'reject',reason:' test reason '}; }, true);
  try {
    assert.deepEqual(await host.approval(op, signal()), {approved:false,reason:'test reason'});
    assert.deepEqual(events,['tool_confirmation']);
  } finally { await host.close(); }
});
