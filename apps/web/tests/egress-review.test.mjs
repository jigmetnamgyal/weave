import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import vm from "node:vm";
import { test } from "node:test";
import ts from "typescript";

const require = createRequire(import.meta.url);

// No DOM/test-library dependency: run the actual TSX with a small hook/host
// model for the lifecycle bugs found in review. This is not a browser or React
// integration test; in particular it does not verify Next's action transport.
function load(relativePath, mocks) {
  const source = readFileSync(fileURLToPath(new URL(relativePath, import.meta.url)), "utf8");
  const compiled = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
  }).outputText;
  const exports = {};
  const jsx = (type, props, key) => ({ type, props, key });
  vm.runInNewContext(compiled, {
    exports,
    crypto: { randomUUID: () => globalThis.crypto.randomUUID() },
    require: (id) => {
      if (id === "react/jsx-runtime") return { jsx, jsxs: jsx };
      if (id in mocks) return mocks[id];
      return require(id);
    },
  });
  return exports;
}

function formHarness(file, exportName) {
  const slots = [];
  let index = 0;
  let effects = [];
  let nodes = new Map();
  const harness = { state: {} };
  const react = {
    useActionState: () => [harness.state, () => {}, false],
    useRef: (initial) => {
      const i = index++;
      slots[i] ??= { current: initial };
      return slots[i];
    },
    useState: (initial) => {
      const i = index++;
      if (!(i in slots)) slots[i] = initial;
      return [slots[i], (value) => (slots[i] = value)];
    },
    useEffect: (effect, deps) => {
      const i = index++;
      if (!slots[i] || deps.some((value, j) => !Object.is(value, slots[i][j]))) {
        effects.push(effect);
      }
      slots[i] = deps;
    },
  };
  const component = load(file, {
    react,
    "@/components/ui/button": { Button: "button" },
    "@/components/ui/input": { Input: "input" },
    "@/app/actions/egress": { addEgressHostAction() {}, removeEgressHostAction() {} },
  })[exportName];
  harness.render = (props) => {
    index = 0;
    effects = [];
    const tree = component(props);
    const next = new Map();
    function visit(element, path) {
      if (!element || typeof element !== "object") return;
      const identity = `${path}:${element.type}:${element.key ?? ""}`;
      const dom = nodes.get(identity) ?? { value: element.props.defaultValue ?? "" };
      dom.props = element.props;
      next.set(identity, dom);
      if (element.props.ref) element.props.ref.current = dom;
      const children = [element.props.children].flat();
      children.forEach((child, i) => visit(child, `${path}.${i}`));
    }
    visit(tree, "root");
    for (const [id, dom] of nodes) {
      if (!next.has(id) && dom.props.ref?.current === dom) dom.props.ref.current = null;
    }
    nodes = next;
    effects.forEach((effect) => effect());
  };
  harness.input = (name) => [...nodes.values()].find((dom) => dom.props.name === name);
  harness.click = (label) => {
    const button = [...nodes.values()].find((dom) => dom.props.children === label);
    assert.ok(button, `button ${label} exists`);
    button.props.onClick();
  };
  harness.type = (value) => {
    const input = harness.input("hostname");
    input.value = value;
    input.props.onChange?.({ currentTarget: input });
  };
  return harness;
}

const addProps = { workspaceId: "workspace", atLimit: false };
function addHarness() {
  return formHarness("../components/add-egress-host-form.tsx", "AddEgressHostForm");
}

// Regression checks for the form and page cases raised in review.
test("add initializes a key when returning from the limit, including initially at limit", () => {
  const h = addHarness();
  h.render({ ...addProps, atLimit: true });
  h.render(addProps);
  assert.match(h.input("idempotency_key").value, /^[a-f0-9-]{36}$/);
});

test("add retry identity survives hiding at the limit", () => {
  const h = addHarness();
  h.render(addProps);
  h.type("docs.example.com");
  h.state = { error: "Lost response", hostname: "docs.example.com" };
  h.render(addProps);
  const key = h.input("idempotency_key").value;
  h.render({ ...addProps, atLimit: true });
  h.render(addProps);
  assert.equal(h.input("idempotency_key").value, key);
  assert.ok(key);
});

test("same input retries its key; editing to a different hostname changes it", () => {
  const h = addHarness();
  h.render(addProps);
  h.type("docs.example.com");
  const key = h.input("idempotency_key").value;
  h.state = { error: "Lost response", hostname: "docs.example.com" };
  h.render(addProps);
  assert.equal(h.input("idempotency_key").value, key);
  h.type(" other.example.com ");
  const changed = h.input("idempotency_key").value;
  assert.notEqual(changed, key);
  h.type("other.example.com");
  assert.equal(h.input("idempotency_key").value, changed);
});

test("edit and revert after an uncertain response restores the original key", () => {
  const h = addHarness();
  h.render(addProps);
  h.type("docs.example.com");
  const original = h.input("idempotency_key").value;
  h.state = { error: "Lost response", hostname: "docs.example.com" };
  h.render(addProps);
  h.type("other.example.com");
  assert.notEqual(h.input("idempotency_key").value, original);
  h.type("docs.example.com");
  assert.equal(h.input("idempotency_key").value, original);
});

test("another host's success does not discard an uncertain host's key", () => {
  const h = addHarness();
  h.render(addProps);
  h.type("docs.example.com");
  const original = h.input("idempotency_key").value;
  h.state = { error: "Lost response", hostname: "docs.example.com" };
  h.render(addProps);
  h.type("other.example.com");
  h.state = { added: "other.example.com" };
  h.render(addProps);
  h.type("docs.example.com");
  assert.equal(h.input("idempotency_key").value, original);
});

test("a confirmed hostname gets a fresh key for its next add", () => {
  const h = addHarness();
  h.render(addProps);
  h.type("docs.example.com");
  const original = h.input("idempotency_key").value;
  h.state = { error: "Lost response", hostname: "docs.example.com" };
  h.render(addProps);
  h.state = { added: "docs.example.com" };
  h.render(addProps);
  h.type("docs.example.com");
  assert.notEqual(h.input("idempotency_key").value, original);
});

test("successful retry clears the field and renews its key", () => {
  const h = addHarness();
  h.render(addProps);
  h.type("docs.example.com");
  h.state = { error: "Lost response", hostname: "docs.example.com" };
  h.render(addProps);
  assert.equal(h.input("hostname").value, "docs.example.com");
  const key = h.input("idempotency_key").value;
  h.state = { added: "docs.example.com" };
  h.render(addProps);
  assert.equal(h.input("hostname").value, "");
  assert.notEqual(h.input("idempotency_key").value, key);
});

test("removal keeps its retry key across Keep and reconfirm", () => {
  const h = formHarness("../components/remove-egress-host-button.tsx", "RemoveEgressHostButton");
  const props = { workspaceId: "workspace", egressHostId: "host", hostname: "docs.example.com" };
  h.render(props);
  h.click("Remove");
  h.render(props);
  const key = h.input("idempotency_key").value;
  h.state = { error: "Lost response" };
  h.render(props);
  h.click("Keep");
  h.render(props);
  h.click("Remove");
  h.render(props);
  assert.equal(h.input("idempotency_key").value, key);
  assert.ok(key);
});

async function pageText(workspaces, hosts) {
  const page = load("../app/(app)/workspaces/[workspaceId]/settings/egress/page.tsx", {
    "next/navigation": {
      notFound() {
        throw new Error("not found");
      },
    },
    "@/components/add-egress-host-form": { AddEgressHostForm: "add-form" },
    "@/components/remove-egress-host-button": { RemoveEgressHostButton: "remove-form" },
    "@/components/ui/card": Object.fromEntries(
      ["Card", "CardContent", "CardDescription", "CardHeader", "CardTitle"].map((name) => [
        name,
        name,
      ])
    ),
    "@/lib/api": { fetchWorkspaces: async () => workspaces, fetchEgressHosts: async () => hosts },
  }).default;
  async function text(node) {
    if (node == null || typeof node === "boolean") return "";
    if (Array.isArray(node)) return (await Promise.all(node.map(text))).join("");
    if (typeof node !== "object") return String(node);
    if (typeof node.type === "function") return text(await node.type(node.props));
    return text(node.props.children);
  }
  return text(await page({ params: Promise.resolve({ workspaceId: "workspace" }) }));
}

test("workspace lookup failure displays its request reference", async () => {
  const result = await pageText({ ok: false, message: "Unavailable", requestId: "req-123" });
  assert.match(result, /Unavailable/);
  assert.match(result, /Reference: req-123/);
});

test("host list includes the creator returned by the API", async () => {
  const result = await pageText(
    { ok: true, data: [{ id: "workspace", name: "Test", permissions: ["workspace:manage"] }] },
    {
      ok: true,
      data: {
        limit: 20,
        items: [
          {
            id: "host",
            hostname: "docs.example.com",
            created_at: "2026-01-01T00:00:00Z",
            created_by: "creator-uuid",
          },
        ],
      },
    }
  );
  assert.match(result, /Added by creator-uuid/);
});
