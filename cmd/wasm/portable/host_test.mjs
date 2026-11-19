import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createHost } from './host.mjs';

const op = {type:'host_call', id:'call-1', name:'add_memory', args:'{"memory":"do not store without approval"}', readOnly:false};
const signal = () => new AbortController().signal;

test('no callback means no write; a negative decision never invokes the handler', async () => {
  const host = createHost({}, undefined, false);
  const emitted = [];
  try {
    const result = await host.execute(op, signal(), e => emitted.push(e));
    assert.deepEqual(result,{is_error:true,output:'The user rejected the tool call.'});
    assert.deepEqual(emitted, []);
    assert.throws(() => host.confirm(true), /no tool confirmation/);
  } finally { await host.close(); }
});

test('binds an approval to the current tool; rejects stale callbacks and wrong tool names', async () => {
  const controller = new AbortController();
  let event;
  const host = createHost({}, e => { event=e; }, false);
  const turn = host.execute(op, controller.signal, () => assert.fail('called after abort'));
  assert.deepEqual(event, {type:'tool_confirmation',id:'call-1',name:op.name,args:op.args});
  assert.throws(() => host.confirm({decision:'approve_tool',toolName:'delete_plan'}), /matching pending/);
  controller.abort();
  await assert.rejects(turn, /aborted/);
  assert.throws(() => host.confirm(true), /no tool confirmation/);
  await host.close();
});

test('stateless approval callbacks can decline asynchronously with a reason', async () => {
  const events=[];
  const host = createHost({}, async e => { events.push(e.type); return {decision:'reject',reason:' test reason '}; }, true);
  try {
    assert.deepEqual(await host.execute(op, signal(), e => events.push(e.type)), {is_error:true,output:'The user rejected the tool call. Reason: test reason'});
    assert.deepEqual(events,['tool_confirmation']);
  } finally { await host.close(); }
});
