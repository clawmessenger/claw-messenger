// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { SessionQueue } from "./run.js";

describe("SessionQueue", () => {
  it("serializes messages within one conversation", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const order: number[] = [];
    const task = (n: number) => async () => {
      order.push(n);
      await new Promise((r) => setTimeout(r, 20));
      order.push(-n);
    };
    q.enqueue("conv-1", task(1));
    q.enqueue("conv-1", task(2));
    await q.idle();
    // task 2 must not start before task 1 finished
    expect(order.indexOf(-1)).toBeLessThan(order.indexOf(2));
  });

  it("runs different conversations in parallel", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const started: string[] = [];
    const slow = (id: string, ms: number) => async () => {
      started.push(id);
      await new Promise((r) => setTimeout(r, ms));
    };
    q.enqueue("conv-a", slow("a", 60));
    q.enqueue("conv-b", slow("b", 10));
    await q.idle();
    // b finished while a was still running -> both started before either idle
    expect(started).toContain("a");
    expect(started).toContain("b");
    expect(started.indexOf("b")).toBeLessThan(2);
  });

  it("busy message replies once when queue is at depth", async () => {
    const onBusy = vi.fn();
    const q = new SessionQueue({ concurrencyPerConversation: 1, queueDepth: 1, onBusy });
    const slow = async () => { await new Promise((r) => setTimeout(r, 40)); };
    q.enqueue("conv-1", slow);
    q.enqueue("conv-1", slow); // queued (depth 1)
    q.enqueue("conv-1", slow); // busy -> onBusy("conv-1")
    await q.idle();
    expect(onBusy).toHaveBeenCalledTimes(1);
    expect(onBusy).toHaveBeenCalledWith("conv-1");
  });

  it("keeps draining when a task throws synchronously", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1 });
    const done: string[] = [];
    q.enqueue("conv-1", (() => { throw new Error("sync boom"); }) as () => Promise<void>);
    q.enqueue("conv-1", async () => { done.push("recovered"); });
    await q.idle();
    expect(done).toEqual(["recovered"]);
  });

  it("idle() resolves after a busy-dropped task leaves the queue", async () => {
    const q = new SessionQueue({ concurrencyPerConversation: 1, queueDepth: 1 });
    let ran = 0;
    const slow = async () => {
      ran++;
      await new Promise((r) => setTimeout(r, 10));
    };
    q.enqueue("conv-1", slow);
    q.enqueue("conv-1", slow);
    q.enqueue("conv-1", slow); // dropped as busy
    await q.idle();
    expect(ran).toBe(2);
    // Queue must accept new work after going idle.
    let ranAfter = false;
    q.enqueue("conv-1", async () => { ranAfter = true; });
    await q.idle();
    expect(ranAfter).toBe(true);
  });
});
