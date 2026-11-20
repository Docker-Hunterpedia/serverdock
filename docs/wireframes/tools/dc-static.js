// Minimal static renderer for the wireframe .dc.html files (no editor runtime needed).
// Supports {{path}} holes, <sc-for>, <sc-if>, <helmet>, and the Component.renderVals() data.
(function () {
  window.DCLogic = class {
    constructor(props) { this.props = props || {}; this.state = {}; }
    setState(s) { Object.assign(this.state, s); }
    forceUpdate() {}
  };
  function lookup(path, scope) {
    path = path.trim();
    if (path === 'true') return true;
    if (path === 'false') return false;
    if (/^-?\d+(\.\d+)?$/.test(path)) return Number(path);
    return path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), scope);
  }
  const HOLE = /\{\{\s*([^}]+?)\s*\}\}/g;
  function interp(str, scope) {
    return str.replace(HOLE, (_, p) => { const v = lookup(p, scope); return v == null || typeof v === 'function' ? '' : String(v); });
  }
  function render(node, scope, out) {
    if (node.nodeType === 3) { out.appendChild(document.createTextNode(interp(node.nodeValue, scope))); return; }
    if (node.nodeType !== 1) return;
    const tag = node.tagName.toLowerCase();
    if (tag === 'helmet') return;
    if (tag === 'sc-for') {
      const list = lookup((node.getAttribute('list') || '').replace(/[{}]/g, ''), scope) || [];
      const as = node.getAttribute('as') || 'item';
      list.forEach((item, i) => { const s = Object.assign({}, scope, { [as]: item, $index: i }); node.childNodes.forEach((c) => render(c, s, out)); });
      return;
    }
    if (tag === 'sc-if') {
      if (lookup((node.getAttribute('value') || '').replace(/[{}]/g, ''), scope)) node.childNodes.forEach((c) => render(c, scope, out));
      return;
    }
    const el = node.namespaceURI === 'http://www.w3.org/1999/xhtml'
      ? document.createElement(node.localName)
      : document.createElementNS(node.namespaceURI, node.localName);
    for (const a of node.attributes) {
      if (/^on/i.test(a.name) || a.name.startsWith('hint-')) continue;
      el.setAttribute(a.name, interp(a.value, scope));
    }
    const src = tag === 'template' ? node.content : node;
    src.childNodes.forEach((c) => render(c, scope, el));
    out.appendChild(el);
  }
  document.addEventListener('DOMContentLoaded', () => {
    const root = document.querySelector('x-dc');
    if (!root) return;
    const helmet = root.querySelector('helmet');
    if (helmet) [...helmet.children].forEach((c) => document.head.appendChild(c.cloneNode(true)));
    const script = document.querySelector('script[data-dc-script]');
    let vals = {};
    if (script) {
      const Component = new Function('DCLogic', script.textContent + '\nreturn Component;')(window.DCLogic);
      vals = new Component({}).renderVals() || {};
    }
    const frag = document.createDocumentFragment();
    root.childNodes.forEach((c) => render(c, vals, frag));
    root.replaceWith(frag);
    document.documentElement.setAttribute('data-h', String(Math.ceil(document.documentElement.scrollHeight)));
  });
})();
