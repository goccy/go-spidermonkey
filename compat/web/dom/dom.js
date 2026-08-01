// compat/web/dom: the document tree (https://dom.spec.whatwg.org/,
// https://html.spec.whatwg.org/).
//
// The tree LIVES here, in the guest: nodes carry expando properties, event
// listeners and identity across mutations, which is exactly the state the
// module rules say belongs where the objects live. The two algorithms worth
// having in Go are in Go, behind ops: dom_parse_html is golang.org/x/net/html
// (the whole HTML5 parsing algorithm), and dom_parse_selector is the
// Selectors grammar; this file builds trees from the former's output and
// walks them with the latter's AST.
//
// Everything installed here is [Exposed=Window]; scope.js strips it from
// worker scopes the way it strips Web Storage.
(() => {
	"use strict";

	const ops = globalThis.__web_ops;
	const HTML_NS = "http://www.w3.org/1999/xhtml";
	const SVG_NS = "http://www.w3.org/2000/svg";
	const MATHML_NS = "http://www.w3.org/1998/Math/MathML";
	const NS_BY_PARSER = { "": HTML_NS, svg: SVG_NS, math: MATHML_NS };

	// Guarded construction: the spec's non-constructible interfaces throw
	// unless this module itself is minting.
	let minting = false;
	function mint(cls) {
		minting = true;
		try { return new cls(); } finally { minting = false; }
	}

	const domError = (msg, name) => new DOMException(msg, name);

	// The environment's document, assigned at the bottom of this file; the
	// constructible node types default their owner to it.
	let mainDocument = null;

	// ------------------------------------------------------------------ Node
	class Node extends EventTarget {
		constructor() {
			super();
			if (!minting && new.target === Node) throw new TypeError("Illegal constructor");
			this._parent = null;
			this._kids = [];
			this._owner = null; // set for everything except a Document itself
		}
		get parentNode() { return this._parent; }
		get parentElement() {
			return this._parent instanceof Element ? this._parent : null;
		}
		get childNodes() {
			// One live view per node: length and indices read _kids as it is NOW.
			if (!this._childNodesView) this._childNodesView = makeNodeList(() => this._kids);
			return this._childNodesView;
		}
		get firstChild() { return this._kids[0] ?? null; }
		get lastChild() { return this._kids[this._kids.length - 1] ?? null; }
		get previousSibling() {
			if (!this._parent) return null;
			const sib = this._parent._kids;
			return sib[sib.indexOf(this) - 1] ?? null;
		}
		get nextSibling() {
			if (!this._parent) return null;
			const sib = this._parent._kids;
			return sib[sib.indexOf(this) + 1] ?? null;
		}
		get ownerDocument() { return this._owner; }
		get isConnected() { return this.getRootNode() instanceof Document; }
		getRootNode() {
			let n = this;
			while (n._parent) n = n._parent;
			return n;
		}
		get baseURI() { return (this._owner ?? this).URL ?? "about:blank"; }
		hasChildNodes() { return this._kids.length > 0; }
		contains(other) {
			for (let n = other; n; n = n._parent) if (n === this) return true;
			return false;
		}
		get nodeValue() { return null; }
		set nodeValue(v) { /* null for this class, per spec */ }
		get textContent() {
			if (this instanceof Document || this instanceof DocumentType) return null;
			let out = "";
			const walk = (n) => {
				for (const k of n._kids) {
					if (k.nodeType === 3) out += k._data;
					else walk(k);
				}
			};
			if (this.nodeType === 3 || this.nodeType === 8 || this.nodeType === 7) return this._data;
			walk(this);
			return out;
		}
		set textContent(v) {
			if (this.nodeType === 3 || this.nodeType === 8 || this.nodeType === 7) {
				this._data = String(v ?? "");
				return;
			}
			if (this instanceof Document || this instanceof DocumentType) return;
			for (const k of [...this._kids]) remove(k);
			const s = String(v ?? "");
			if (s !== "") insert(makeText(ownerOf(this), s), this, null);
		}
		normalize() {
			for (const k of [...this._kids]) {
				if (k.nodeType === 3) {
					if (k._data === "") { remove(k); continue; }
					let next = k.nextSibling;
					while (next && next.nodeType === 3) {
						k._data += next._data;
						const gone = next;
						next = next.nextSibling;
						remove(gone);
					}
				} else {
					k.normalize();
				}
			}
		}
		cloneNode(deep = false) { return cloneNode(this, Boolean(deep)); }
		isSameNode(other) { return other === this; }
		appendChild(node) { return preInsert(node, this, null); }
		insertBefore(node, child) {
			if (arguments.length < 2) throw new TypeError("insertBefore: two arguments required");
			return preInsert(node, this, child ?? null);
		}
		removeChild(child) {
			if (!(child instanceof Node) || child._parent !== this) {
				throw domError("the node is not a child of this node", "NotFoundError");
			}
			remove(child);
			return child;
		}
		replaceChild(node, child) {
			if (!(child instanceof Node) || child._parent !== this) {
				throw domError("the node is not a child of this node", "NotFoundError");
			}
			validateInsert(node, this, child);
			const anchor = child.nextSibling;
			remove(child);
			preInsert(node, this, anchor === child ? null : anchor);
			return child;
		}
	}
	for (const [name, value] of [
		["ELEMENT_NODE", 1], ["ATTRIBUTE_NODE", 2], ["TEXT_NODE", 3],
		["CDATA_SECTION_NODE", 4], ["ENTITY_REFERENCE_NODE", 5], ["ENTITY_NODE", 6],
		["PROCESSING_INSTRUCTION_NODE", 7], ["COMMENT_NODE", 8], ["DOCUMENT_NODE", 9],
		["DOCUMENT_TYPE_NODE", 10], ["DOCUMENT_FRAGMENT_NODE", 11], ["NOTATION_NODE", 12],
	]) {
		for (const target of [Node, Node.prototype]) {
			Object.defineProperty(target, name, { value, enumerable: true });
		}
	}

	// ownerOf: the document a new node created "in the context of" n belongs to.
	const ownerOf = (n) => (n instanceof Document ? n : n._owner) ?? mainDocument;

	// ------------------------------------------------- mutation algorithms
	function validateInsert(node, parent, before) {
		if (!(node instanceof Node)) throw new TypeError("the child is not a Node");
		if (!(parent instanceof Element || parent instanceof Document || parent instanceof DocumentFragment)) {
			throw domError("this node cannot have children", "HierarchyRequestError");
		}
		for (let n = parent; n; n = n._parent) {
			if (n === node) throw domError("a node cannot contain itself", "HierarchyRequestError");
		}
		if (before !== null && before._parent !== parent) {
			throw domError("the reference child is not a child of this node", "NotFoundError");
		}
		if (node instanceof Document) {
			throw domError("a document cannot be a child", "HierarchyRequestError");
		}
		if (node instanceof DocumentType && !(parent instanceof Document)) {
			throw domError("a doctype belongs to a document", "HierarchyRequestError");
		}
		if (parent instanceof Document) {
			const wouldAddElements = node instanceof Element ? 1
				: node instanceof DocumentFragment ? node._kids.filter((k) => k instanceof Element).length : 0;
			const have = parent._kids.filter((k) => k instanceof Element).length;
			if (wouldAddElements + have > 1) {
				throw domError("a document can have one element child", "HierarchyRequestError");
			}
			if (node.nodeType === 3 || (node instanceof DocumentFragment && node._kids.some((k) => k.nodeType === 3))) {
				throw domError("a document cannot contain text", "HierarchyRequestError");
			}
		}
	}

	function adopt(node, doc) {
		node._owner = doc;
		for (const k of node._kids) adopt(k, doc);
		if (node._attrs) for (const a of node._attrs) a._owner = doc;
	}

	function insert(node, parent, before) {
		const doc = parent instanceof Document ? parent : parent._owner;
		if (node instanceof DocumentFragment) {
			for (const k of [...node._kids]) insert(k, parent, before);
			return;
		}
		if (node._parent) remove(node);
		if (doc && node._owner !== doc) adopt(node, doc);
		const i = before === null ? parent._kids.length : parent._kids.indexOf(before);
		parent._kids.splice(i, 0, node);
		node._parent = parent;
	}

	function preInsert(node, parent, before) {
		validateInsert(node, parent, before);
		if (before === node) before = node.nextSibling;
		insert(node, parent, before);
		return node;
	}

	function remove(node) {
		const p = node._parent;
		if (!p) return;
		const i = p._kids.indexOf(node);
		if (i >= 0) p._kids.splice(i, 1);
		node._parent = null;
	}

	function cloneNode(node, deep) {
		let copy;
		switch (node.nodeType) {
			case 1: {
				copy = makeElement(node._owner, node._local, node._ns);
				for (const a of node._attrs) {
					setAttrRaw(copy, a._local, a._ns, a._prefix, a._value);
				}
				break;
			}
			case 3: copy = makeText(node._owner, node._data); break;
			case 8: copy = makeComment(node._owner, node._data); break;
			case 7: copy = makePI(node._owner, node._piTarget, node._data); break;
			case 9: {
				copy = mint(Document);
				copy._contentType = node._contentType;
				copy._url = node._url;
				break;
			}
			case 10: copy = makeDoctype(node._owner, node._name); break;
			case 11: copy = mint(DocumentFragment); copy._owner = node._owner; break;
			default: throw domError("this node cannot be cloned", "NotSupportedError");
		}
		if (deep) {
			for (const k of node._kids) {
				insert(cloneNode(k, true), copy, null);
			}
		}
		return copy;
	}

	// ---------------------------------------------------------- tree walks
	function* descendants(root) {
		for (const k of root._kids) {
			yield k;
			yield* descendants(k);
		}
	}
	function* elementDescendants(root) {
		for (const n of descendants(root)) if (n instanceof Element) yield n;
	}

	// ------------------------------------------------------- CharacterData
	class CharacterData extends Node {
		constructor() {
			if (!minting && new.target === CharacterData) throw new TypeError("Illegal constructor");
			super();
			this._data = "";
		}
		get data() { return this._data; }
		set data(v) { this._data = String(v ?? ""); }
		get nodeValue() { return this._data; }
		set nodeValue(v) { this._data = String(v ?? ""); }
		get length() { return this._data.length; }
		substringData(offset, count) {
			offset = index(offset, this._data.length, "substringData");
			return this._data.substr(offset, count);
		}
		appendData(s) { this._data += String(s); }
		insertData(offset, s) {
			offset = index(offset, this._data.length, "insertData");
			this._data = this._data.slice(0, offset) + String(s) + this._data.slice(offset);
		}
		deleteData(offset, count) {
			offset = index(offset, this._data.length, "deleteData");
			this._data = this._data.slice(0, offset) + this._data.slice(offset + Math.max(0, count));
		}
		replaceData(offset, count, s) {
			offset = index(offset, this._data.length, "replaceData");
			this._data = this._data.slice(0, offset) + String(s) + this._data.slice(offset + Math.max(0, count));
		}
	}
	function index(offset, len, op) {
		offset = Number(offset) >>> 0;
		if (offset > len) throw domError(`${op}: the offset is past the end`, "IndexSizeError");
		return offset;
	}

	class Text extends CharacterData {
		constructor(data = "") {
			const wasMinting = minting;
			minting = true;
			super();
			minting = wasMinting;
			this._data = String(data);
			this._owner = mainDocument;
		}
		get nodeType() { return 3; }
		get nodeName() { return "#text"; }
		get wholeText() {
			let start = this;
			while (start.previousSibling && start.previousSibling.nodeType === 3) start = start.previousSibling;
			let out = "";
			for (let n = start; n && n.nodeType === 3; n = n.nextSibling) out += n._data;
			return out;
		}
		splitText(offset) {
			offset = index(offset, this._data.length, "splitText");
			const rest = makeText(this._owner, this._data.slice(offset));
			this._data = this._data.slice(0, offset);
			if (this._parent) insert(rest, this._parent, this.nextSibling);
			return rest;
		}
	}

	class Comment extends CharacterData {
		constructor(data = "") {
			const wasMinting = minting;
			minting = true;
			super();
			minting = wasMinting;
			this._data = String(data);
			this._owner = mainDocument;
		}
		get nodeType() { return 8; }
		get nodeName() { return "#comment"; }
	}

	class ProcessingInstruction extends CharacterData {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			super();
			this._piTarget = "";
		}
		get nodeType() { return 7; }
		get nodeName() { return this._piTarget; }
		get target() { return this._piTarget; }
	}

	class DocumentType extends Node {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			super();
			this._name = "";
		}
		get nodeType() { return 10; }
		get nodeName() { return this._name; }
		get name() { return this._name; }
		get publicId() { return ""; }
		get systemId() { return ""; }
	}

	// ------------------------------------------------------------ Attr
	class Attr extends Node {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			super();
			this._local = "";
			this._ns = null;
			this._prefix = null;
			this._value = "";
			this._element = null;
		}
		get nodeType() { return 2; }
		get localName() { return this._local; }
		get name() { return this._prefix ? this._prefix + ":" + this._local : this._local; }
		get nodeName() { return this.name; }
		get namespaceURI() { return this._ns; }
		get prefix() { return this._prefix; }
		get value() { return this._value; }
		set value(v) { this._value = String(v); }
		get nodeValue() { return this._value; }
		set nodeValue(v) { this._value = String(v); }
		get ownerElement() { return this._element; }
		get specified() { return true; }
	}

	class NamedNodeMap {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			this._el = null;
		}
		get length() { return this._el._attrs.length; }
		item(i) { return this._el._attrs[Number(i) >>> 0] ?? null; }
		getNamedItem(name) {
			name = String(name).toLowerCase();
			return this._el._attrs.find((a) => a.name === name) ?? null;
		}
		getNamedItemNS(ns, local) {
			ns = ns === "" ? null : ns;
			return this._el._attrs.find((a) => a._ns === ns && a._local === String(local)) ?? null;
		}
		setNamedItem(attr) {
			if (!(attr instanceof Attr)) throw new TypeError("setNamedItem: an Attr is required");
			const old = this.getNamedItemNS(attr._ns, attr._local);
			if (old === attr) return attr;
			if (old) this._el._attrs.splice(this._el._attrs.indexOf(old), 1);
			attr._element = this._el;
			this._el._attrs.push(attr);
			return old;
		}
		removeNamedItem(name) {
			const a = this.getNamedItem(name);
			if (!a) throw domError("no attribute named " + name, "NotFoundError");
			this._el._attrs.splice(this._el._attrs.indexOf(a), 1);
			a._element = null;
			return a;
		}
		*[Symbol.iterator]() { yield* this._el._attrs; }
	}

	// ------------------------------------------------------- DOMTokenList
	const splitTokens = (v) => String(v ?? "").split(/[ \t\n\f\r]+/).filter(Boolean);
	class DOMTokenList {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			this._el = null;
			this._attr = "";
		}
		_tokens() {
			const seen = [];
			for (const t of splitTokens(this._el.getAttribute(this._attr))) {
				if (!seen.includes(t)) seen.push(t);
			}
			return seen;
		}
		_write(tokens) { this._el.setAttribute(this._attr, tokens.join(" ")); }
		_check(token, op) {
			token = String(token);
			if (token === "") throw domError(`${op}: the token is empty`, "SyntaxError");
			if (/[ \t\n\f\r]/.test(token)) throw domError(`${op}: the token has whitespace`, "InvalidCharacterError");
			return token;
		}
		get length() { return this._tokens().length; }
		item(i) { return this._tokens()[Number(i) >>> 0] ?? null; }
		contains(token) { return this._tokens().includes(String(token)); }
		add(...tokens) {
			const list = this._tokens();
			for (const raw of tokens) {
				const t = this._check(raw, "add");
				if (!list.includes(t)) list.push(t);
			}
			this._write(list);
		}
		remove(...tokens) {
			let list = this._tokens();
			for (const raw of tokens) {
				const t = this._check(raw, "remove");
				list = list.filter((x) => x !== t);
			}
			this._write(list);
		}
		toggle(token, force) {
			const t = this._check(token, "toggle");
			const has = this.contains(t);
			if (has && force !== true) { this.remove(t); return false; }
			if (!has && force !== false) { this.add(t); return true; }
			return has;
		}
		replace(oldToken, newToken) {
			const o = this._check(oldToken, "replace");
			const n = this._check(newToken, "replace");
			const list = this._tokens();
			const i = list.indexOf(o);
			if (i < 0) return false;
			list[i] = n;
			this._write(list.filter((t, j) => list.indexOf(t) === j));
			return true;
		}
		supports() { return true; }
		get value() { return this._el.getAttribute(this._attr) ?? ""; }
		set value(v) { this._el.setAttribute(this._attr, String(v)); }
		toString() { return this.value; }
		forEach(fn, thisArg) { this._tokens().forEach((t, i) => fn.call(thisArg, t, i, this)); }
		*[Symbol.iterator]() { yield* this._tokens(); }
		keys() { return this._tokens().keys(); }
		values() { return this._tokens().values(); }
		entries() { return this._tokens().entries(); }
	}

	// ------------------------------------------------------------ Element
	class Element extends Node {
		constructor() {
			if (!minting && (new.target === Element)) throw new TypeError("Illegal constructor");
			super();
			this._local = "";
			this._ns = HTML_NS;
			this._prefix = null;
			this._attrs = [];
		}
		get nodeType() { return 1; }
		get localName() { return this._local; }
		get namespaceURI() { return this._ns; }
		get prefix() { return this._prefix; }
		get tagName() {
			const q = this._prefix ? this._prefix + ":" + this._local : this._local;
			return this._ns === HTML_NS ? q.toUpperCase() : q;
		}
		get nodeName() { return this.tagName; }
		get attributes() {
			if (!this._attrMap) {
				this._attrMap = mint(NamedNodeMap);
				this._attrMap._el = this;
			}
			return this._attrMap;
		}
		_findAttr(qname) {
			if (this._ns === HTML_NS) qname = String(qname).toLowerCase();
			return this._attrs.find((a) => a.name === qname) ?? null;
		}
		getAttribute(qname) { return this._findAttr(qname)?._value ?? null; }
		getAttributeNS(ns, local) {
			ns = ns === "" ? null : ns;
			return this._attrs.find((a) => a._ns === ns && a._local === String(local))?._value ?? null;
		}
		hasAttribute(qname) { return this._findAttr(qname) !== null; }
		hasAttributeNS(ns, local) { return this.getAttributeNS(ns, local) !== null; }
		hasAttributes() { return this._attrs.length > 0; }
		getAttributeNames() { return this._attrs.map((a) => a.name); }
		setAttribute(qname, value) {
			qname = String(qname);
			if (!/^[^\t\n\f\r />"'=\0]+$/.test(qname) || /[ <]/.test(qname)) {
				throw domError(`"${qname}" is not a valid attribute name`, "InvalidCharacterError");
			}
			if (this._ns === HTML_NS) qname = qname.toLowerCase();
			const a = this._findAttr(qname);
			if (a) { a._value = String(value); return; }
			setAttrRaw(this, qname, null, null, String(value));
		}
		setAttributeNS(ns, qname, value) {
			ns = ns === "" || ns === undefined ? null : ns === null ? null : String(ns);
			qname = String(qname);
			let prefix = null, local = qname;
			const colon = qname.indexOf(":");
			if (colon >= 0) { prefix = qname.slice(0, colon); local = qname.slice(colon + 1); }
			if (prefix !== null && ns === null) {
				throw domError("a prefixed attribute needs a namespace", "NamespaceError");
			}
			const a = this._attrs.find((x) => x._ns === ns && x._local === local);
			if (a) { a._value = String(value); return; }
			setAttrRaw(this, local, ns, prefix, String(value));
		}
		removeAttribute(qname) {
			const a = this._findAttr(qname);
			if (a) { this._attrs.splice(this._attrs.indexOf(a), 1); a._element = null; }
		}
		removeAttributeNS(ns, local) {
			ns = ns === "" ? null : ns;
			const a = this._attrs.find((x) => x._ns === ns && x._local === String(local));
			if (a) { this._attrs.splice(this._attrs.indexOf(a), 1); a._element = null; }
		}
		toggleAttribute(qname, force) {
			qname = String(qname);
			if (this.hasAttribute(qname)) {
				if (force === true) return true;
				this.removeAttribute(qname);
				return false;
			}
			if (force === false) return false;
			this.setAttribute(qname, "");
			return true;
		}
		getAttributeNode(qname) { return this._findAttr(qname); }
		setAttributeNode(attr) { return this.attributes.setNamedItem(attr); }
		removeAttributeNode(attr) {
			const i = this._attrs.indexOf(attr);
			if (i < 0) throw domError("the attribute is not here", "NotFoundError");
			this._attrs.splice(i, 1);
			attr._element = null;
			return attr;
		}
		get id() { return this.getAttribute("id") ?? ""; }
		set id(v) { this.setAttribute("id", v); }
		get className() { return this.getAttribute("class") ?? ""; }
		set className(v) { this.setAttribute("class", v); }
		get classList() {
			if (!this._classList) {
				this._classList = mint(DOMTokenList);
				this._classList._el = this;
				this._classList._attr = "class";
			}
			return this._classList;
		}
		get children() {
			if (!this._childrenView) this._childrenView = makeHTMLCollection(() => this._kids.filter((k) => k instanceof Element));
			return this._childrenView;
		}
		get firstElementChild() { return this._kids.find((k) => k instanceof Element) ?? null; }
		get lastElementChild() {
			for (let i = this._kids.length - 1; i >= 0; i--) if (this._kids[i] instanceof Element) return this._kids[i];
			return null;
		}
		get childElementCount() { return this._kids.filter((k) => k instanceof Element).length; }
		get previousElementSibling() {
			for (let n = this.previousSibling; n; n = n.previousSibling) if (n instanceof Element) return n;
			return null;
		}
		get nextElementSibling() {
			for (let n = this.nextSibling; n; n = n.nextSibling) if (n instanceof Element) return n;
			return null;
		}
		get innerHTML() { return serializeChildren(this); }
		set innerHTML(v) {
			for (const k of [...this._kids]) remove(k);
			buildParsedInto(this, String(v ?? ""));
		}
		get outerHTML() { return serializeNode(this); }
		set outerHTML(v) {
			const p = this._parent;
			if (!p) return;
			const frag = mint(DocumentFragment);
			frag._owner = this._owner;
			buildParsedInto(frag, String(v ?? ""), p instanceof Element ? p._local : "body");
			preInsert(frag, p, this);
			remove(this);
		}
		insertAdjacentHTML(position, text) {
			position = String(position).toLowerCase();
			const frag = mint(DocumentFragment);
			frag._owner = this._owner;
			const ctx = (position === "beforebegin" || position === "afterend")
				? (this._parent instanceof Element ? this._parent._local : "body")
				: this._local;
			buildParsedInto(frag, String(text ?? ""), ctx);
			switch (position) {
				case "beforebegin":
					if (this._parent) preInsert(frag, this._parent, this);
					break;
				case "afterend":
					if (this._parent) preInsert(frag, this._parent, this.nextSibling);
					break;
				case "afterbegin": preInsert(frag, this, this.firstChild); break;
				case "beforeend": preInsert(frag, this, null); break;
				default: throw domError(`"${position}" is not an insertion position`, "SyntaxError");
			}
		}
		insertAdjacentText(position, data) {
			const t = makeText(ownerOf(this), String(data));
			this._insertAdjacentNode(String(position).toLowerCase(), t);
		}
		insertAdjacentElement(position, el) {
			if (!(el instanceof Element)) throw new TypeError("insertAdjacentElement: an Element is required");
			return this._insertAdjacentNode(String(position).toLowerCase(), el);
		}
		_insertAdjacentNode(position, node) {
			switch (position) {
				case "beforebegin":
					if (!this._parent) return null;
					return preInsert(node, this._parent, this);
				case "afterend":
					if (!this._parent) return null;
					return preInsert(node, this._parent, this.nextSibling);
				case "afterbegin": return preInsert(node, this, this.firstChild);
				case "beforeend": return preInsert(node, this, null);
				default: throw domError(`"${position}" is not an insertion position`, "SyntaxError");
			}
		}
		matches(selectors) { return matchesSelector(this, compileSelector(selectors), this); }
		webkitMatchesSelector(selectors) { return this.matches(selectors); }
		closest(selectors) {
			// :scope inside closest() means the element closest() was called
			// on, whichever ancestor is being tried.
			const ast = compileSelector(selectors);
			for (let n = this; n instanceof Element; n = n._parent) {
				if (matchesSelector(n, ast, this)) return n;
			}
			return null;
		}
		getElementsByTagName(name) { return byTagName(this, name); }
		getElementsByTagNameNS(ns, name) { return byTagNameNS(this, ns, name); }
		getElementsByClassName(names) { return byClassName(this, names); }
		get style() {
			if (!this._style) this._style = makeStyleBag(this);
			return this._style;
		}
		set style(v) { this.setAttribute("style", String(v)); }
	}

	// ----------------------------------------------- ChildNode / ParentNode
	function nodesFrom(owner, args) {
		const frag = mint(DocumentFragment);
		frag._owner = owner;
		for (const a of args) {
			insert(a instanceof Node ? a : makeText(owner, String(a)), frag, null);
		}
		return frag;
	}
	const ChildNode = {
		remove() { remove(this); },
		before(...nodes) {
			const p = this._parent;
			if (!p) return;
			let anchor = this;
			while (anchor && nodes.includes(anchor)) anchor = anchor.previousSibling;
			preInsert(nodesFrom(ownerOf(p), nodes), p, anchor ? anchor.nextSibling : p.firstChild);
		},
		after(...nodes) {
			const p = this._parent;
			if (!p) return;
			let anchor = this.nextSibling;
			while (anchor && nodes.includes(anchor)) anchor = anchor.nextSibling;
			preInsert(nodesFrom(ownerOf(p), nodes), p, anchor);
		},
		replaceWith(...nodes) {
			const p = this._parent;
			if (!p) return;
			let anchor = this.nextSibling;
			while (anchor && nodes.includes(anchor)) anchor = anchor.nextSibling;
			remove(this);
			preInsert(nodesFrom(ownerOf(p), nodes), p, anchor);
		},
	};
	const ParentNode = {
		append(...nodes) { preInsert(nodesFrom(ownerOf(this), nodes), this, null); },
		prepend(...nodes) { preInsert(nodesFrom(ownerOf(this), nodes), this, this.firstChild); },
		replaceChildren(...nodes) {
			const frag = nodesFrom(ownerOf(this), nodes);
			for (const k of [...this._kids]) remove(k);
			preInsert(frag, this, null);
		},
		querySelector(selectors) {
			const ast = compileSelector(selectors);
			for (const el of elementDescendants(this)) {
				if (matchesSelector(el, ast, this)) return el;
			}
			return null;
		},
		querySelectorAll(selectors) {
			const ast = compileSelector(selectors);
			const out = [];
			for (const el of elementDescendants(this)) {
				if (matchesSelector(el, ast, this)) out.push(el);
			}
			return makeNodeList(() => out);
		},
	};
	Object.assign(Element.prototype, ChildNode, ParentNode);
	Object.assign(CharacterData.prototype, ChildNode);
	Object.assign(DocumentType.prototype, { remove: ChildNode.remove });

	// ------------------------------------------------------- HTML elements
	class HTMLElement extends Element {
		constructor() {
			if (!minting && new.target === HTMLElement) throw new TypeError("Illegal constructor");
			super();
		}
		get title() { return this.getAttribute("title") ?? ""; }
		set title(v) { this.setAttribute("title", v); }
		get lang() { return this.getAttribute("lang") ?? ""; }
		set lang(v) { this.setAttribute("lang", v); }
		get dir() { return this.getAttribute("dir") ?? ""; }
		set dir(v) { this.setAttribute("dir", v); }
		get hidden() { return this.hasAttribute("hidden"); }
		set hidden(v) { if (v) this.setAttribute("hidden", ""); else this.removeAttribute("hidden"); }
		get innerText() { return this.textContent; }
		set innerText(v) { this.textContent = v; }
		get dataset() {
			if (!this._dataset) this._dataset = makeDataset(this);
			return this._dataset;
		}
		click() {
			const ev = new Event("click", { bubbles: true, cancelable: true });
			this.dispatchEvent(ev);
		}
	}
	class HTMLUnknownElement extends HTMLElement {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			super();
		}
	}
	const simpleHTMLClass = (name) => {
		const cls = class extends HTMLElement {
			constructor() {
				if (!minting) throw new TypeError("Illegal constructor");
				super();
			}
		};
		Object.defineProperty(cls, "name", { value: name, configurable: true });
		return cls;
	};
	const HTMLHtmlElement = simpleHTMLClass("HTMLHtmlElement");
	const HTMLHeadElement = simpleHTMLClass("HTMLHeadElement");
	const HTMLBodyElement = simpleHTMLClass("HTMLBodyElement");
	const HTMLDivElement = simpleHTMLClass("HTMLDivElement");
	const HTMLSpanElement = simpleHTMLClass("HTMLSpanElement");
	const HTMLTitleElement = simpleHTMLClass("HTMLTitleElement");

	class HTMLScriptElement extends HTMLElement {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			super();
		}
		get src() {
			const v = this.getAttribute("src");
			if (v === null) return "";
			try { return new URL(v, this._owner?.URL ?? "about:blank").href; } catch { return v; }
		}
		set src(v) { this.setAttribute("src", v); }
		get type() { return this.getAttribute("type") ?? ""; }
		set type(v) { this.setAttribute("type", v); }
		get text() { return this.textContent; }
		set text(v) { this.textContent = v; }
		get async() { return this.hasAttribute("async"); }
		set async(v) { if (v) this.setAttribute("async", ""); else this.removeAttribute("async"); }
		get defer() { return this.hasAttribute("defer"); }
		set defer(v) { if (v) this.setAttribute("defer", ""); else this.removeAttribute("defer"); }
	}

	// HTMLAnchorElement carries HyperlinkElementUtils: the URL accessors are a
	// live parse of the href attribute against the document's URL — the URL
	// machinery this platform already has.
	class HTMLAnchorElement extends HTMLElement {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
			super();
		}
		_url() {
			const raw = this.getAttribute("href");
			if (raw === null) return null;
			try { return new URL(raw, this._owner?.URL ?? "about:blank"); } catch { return null; }
		}
		get href() {
			const u = this._url();
			return u ? u.href : (this.getAttribute("href") ?? "");
		}
		set href(v) { this.setAttribute("href", v); }
		toString() { return this.href; }
		get target() { return this.getAttribute("target") ?? ""; }
		set target(v) { this.setAttribute("target", v); }
		get text() { return this.textContent; }
		set text(v) { this.textContent = v; }
	}
	for (const part of ["origin", "protocol", "username", "password", "host",
		"hostname", "port", "pathname", "search", "hash"]) {
		Object.defineProperty(HTMLAnchorElement.prototype, part, {
			get() {
				const u = this._url();
				return u ? u[part] : "";
			},
			set(v) {
				if (part === "origin") return;
				const u = this._url();
				if (!u) return;
				u[part] = v;
				this.setAttribute("href", u.href);
			},
			enumerable: true, configurable: true,
		});
	}

	const HTML_CLASSES = {
		html: HTMLHtmlElement, head: HTMLHeadElement, body: HTMLBodyElement,
		div: HTMLDivElement, span: HTMLSpanElement, title: HTMLTitleElement,
		script: HTMLScriptElement, a: HTMLAnchorElement,
	};
	// The tags the parser knows are real HTML elements; anything else in the
	// HTML namespace is HTMLUnknownElement, per the spec's element interfaces.
	const KNOWN_HTML = new Set(("html head body title base link meta style script noscript template slot " +
		"article section nav aside h1 h2 h3 h4 h5 h6 hgroup header footer address p hr pre blockquote " +
		"ol ul menu li dl dt dd figure figcaption main search div a em strong small s cite q dfn abbr " +
		"ruby rt rp data time code var samp kbd sub sup i b u mark bdi bdo span br wbr ins del picture " +
		"source img iframe embed object param video audio track map area table caption colgroup col " +
		"tbody thead tfoot tr td th form label input button select datalist optgroup option textarea " +
		"output progress meter fieldset legend details summary dialog canvas").split(" "));

	function makeElement(doc, local, ns, prefix = null) {
		let cls;
		if (ns === HTML_NS) {
			cls = HTML_CLASSES[local] ?? (KNOWN_HTML.has(local) ? HTMLElement : HTMLUnknownElement);
		} else {
			cls = Element;
		}
		const el = mint(cls);
		el._local = local;
		el._ns = ns;
		el._prefix = prefix;
		el._owner = doc;
		return el;
	}
	function setAttrRaw(el, local, ns, prefix, value) {
		const a = mint(Attr);
		a._local = local;
		a._ns = ns;
		a._prefix = prefix;
		a._value = value;
		a._element = el;
		a._owner = el._owner;
		el._attrs.push(a);
	}
	function makeText(doc, data) {
		const t = mint(Text);
		t._data = data;
		t._owner = doc;
		return t;
	}
	function makeComment(doc, data) {
		const c = mint(Comment);
		c._data = data;
		c._owner = doc;
		return c;
	}
	function makePI(doc, target, data) {
		const pi = mint(ProcessingInstruction);
		pi._piTarget = target;
		pi._data = data;
		pi._owner = doc;
		return pi;
	}
	function makeDoctype(doc, name) {
		const dt = mint(DocumentType);
		dt._name = name;
		dt._owner = doc;
		return dt;
	}

	// -------------------------------------------------------- collections
	// Live views: length and indices recompute from the backing closure on
	// every access, which is what "live" means without a mutation log.
	function collectionProxy(target, items) {
		return new Proxy(target, {
			get(t, p, r) {
				if (typeof p === "string" && p !== "" && !(p in t)) {
					const i = Number(p);
					if (Number.isInteger(i) && i >= 0) return items()[i];
				}
				return Reflect.get(t, p, r);
			},
			has(t, p) {
				if (typeof p === "string") {
					const i = Number(p);
					if (Number.isInteger(i) && i >= 0) return i < items().length;
				}
				return Reflect.has(t, p);
			},
			ownKeys(t) {
				const keys = [];
				for (let i = 0; i < items().length; i++) keys.push(String(i));
				return [...keys, ...Reflect.ownKeys(t)];
			},
			getOwnPropertyDescriptor(t, p) {
				if (typeof p === "string") {
					const i = Number(p);
					if (Number.isInteger(i) && i >= 0 && i < items().length) {
						return { value: items()[i], writable: false, enumerable: true, configurable: true };
					}
				}
				return Reflect.getOwnPropertyDescriptor(t, p);
			},
		});
	}
	class NodeList {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
		}
		get length() { return this._items().length; }
		item(i) { return this._items()[Number(i) >>> 0] ?? null; }
		forEach(fn, thisArg) { this._items().forEach((n, i) => fn.call(thisArg, n, i, this)); }
		*[Symbol.iterator]() { yield* this._items(); }
		keys() { return this._items().keys(); }
		values() { return this._items().values(); }
		entries() { return this._items().entries(); }
	}
	function makeNodeList(items) {
		const list = mint(NodeList);
		list._items = items;
		return collectionProxy(list, items);
	}
	class HTMLCollection {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
		}
		get length() { return this._items().length; }
		item(i) { return this._items()[Number(i) >>> 0] ?? null; }
		namedItem(name) {
			name = String(name);
			if (name === "") return null;
			return this._items().find((el) => el.id === name || el.getAttribute("name") === name) ?? null;
		}
		*[Symbol.iterator]() { yield* this._items(); }
	}
	function makeHTMLCollection(items) {
		const col = mint(HTMLCollection);
		col._items = items;
		return collectionProxy(col, items);
	}
	function byTagName(root, name) {
		name = String(name);
		const lower = name.toLowerCase();
		return makeHTMLCollection(() => {
			const out = [];
			for (const el of elementDescendants(root)) {
				if (name === "*" || (el._ns === HTML_NS ? el._local === lower : el._local === name)) out.push(el);
			}
			return out;
		});
	}
	function byTagNameNS(root, ns, name) {
		ns = ns === "" ? null : ns;
		name = String(name);
		return makeHTMLCollection(() => {
			const out = [];
			for (const el of elementDescendants(root)) {
				if ((ns === "*" || el._ns === ns) && (name === "*" || el._local === name)) out.push(el);
			}
			return out;
		});
	}
	function byClassName(root, names) {
		const want = splitTokens(names);
		return makeHTMLCollection(() => {
			if (want.length === 0) return [];
			const out = [];
			for (const el of elementDescendants(root)) {
				const have = splitTokens(el.getAttribute("class"));
				if (want.every((w) => have.includes(w))) out.push(el);
			}
			return out;
		});
	}

	// ------------------------------------------------------------ style bag
	// A deliberately small stand-in for CSSStyleDeclaration: a property map
	// that serializes to the style attribute. Layout and computed style wait
	// for a CSS module; this keeps `el.style.color = ...` working and honest.
	function makeStyleBag(el) {
		const read = () => {
			const m = new Map();
			for (const decl of String(el.getAttribute("style") ?? "").split(";")) {
				const [k, ...v] = decl.split(":");
				if (k && v.length) m.set(k.trim().toLowerCase(), v.join(":").trim());
			}
			return m;
		};
		const write = (m) => {
			const s = [...m.entries()].map(([k, v]) => `${k}: ${v};`).join(" ");
			if (s) el.setAttribute("style", s);
			else el.removeAttribute("style");
		};
		const kebab = (p) => p.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase());
		const bag = {
			getPropertyValue: (p) => read().get(String(p).toLowerCase()) ?? "",
			setProperty(p, v) {
				const m = read();
				m.set(String(p).toLowerCase(), String(v));
				write(m);
			},
			removeProperty(p) {
				const m = read();
				const old = m.get(String(p).toLowerCase()) ?? "";
				m.delete(String(p).toLowerCase());
				write(m);
				return old;
			},
			get cssText() { return el.getAttribute("style") ?? ""; },
			set cssText(v) { el.setAttribute("style", String(v)); },
			get length() { return read().size; },
			item(i) { return [...read().keys()][Number(i) >>> 0] ?? ""; },
		};
		return new Proxy(bag, {
			get(t, p, r) {
				if (typeof p === "string" && !(p in t)) return t.getPropertyValue(kebab(p));
				return Reflect.get(t, p, r);
			},
			set(t, p, v, r) {
				if (typeof p === "string" && !(p in t)) { t.setProperty(kebab(p), v); return true; }
				return Reflect.set(t, p, v, r);
			},
		});
	}

	function makeDataset(el) {
		const toAttr = (p) => "data-" + p.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase());
		return new Proxy({}, {
			get(t, p) {
				if (typeof p !== "string") return undefined;
				return el.getAttribute(toAttr(p)) ?? undefined;
			},
			set(t, p, v) {
				if (typeof p === "string") el.setAttribute(toAttr(p), String(v));
				return true;
			},
			deleteProperty(t, p) {
				if (typeof p === "string") el.removeAttribute(toAttr(p));
				return true;
			},
			has(t, p) { return typeof p === "string" && el.hasAttribute(toAttr(p)); },
		});
	}

	// ------------------------------------------------------------ Document
	class DocumentFragment extends Node {
		constructor() {
			const wasMinting = minting;
			minting = true;
			super();
			minting = wasMinting;
			this._owner = mainDocument;
		}
		get nodeType() { return 11; }
		get nodeName() { return "#document-fragment"; }
		getElementById(id) {
			id = String(id);
			if (id === "") return null;
			for (const el of elementDescendants(this)) if (el.id === id) return el;
			return null;
		}
	}
	Object.assign(DocumentFragment.prototype, ParentNode);
	Object.defineProperty(DocumentFragment.prototype, "children", {
		get() {
			if (!this._childrenView) this._childrenView = makeHTMLCollection(() => this._kids.filter((k) => k instanceof Element));
			return this._childrenView;
		},
		enumerable: true, configurable: true,
	});
	for (const g of ["firstElementChild", "lastElementChild", "childElementCount"]) {
		Object.defineProperty(DocumentFragment.prototype, g,
			Object.getOwnPropertyDescriptor(Element.prototype, g));
	}

	class DOMImplementation {
		constructor() {
			if (!minting) throw new TypeError("Illegal constructor");
		}
		hasFeature() { return true; }
		createDocumentType(name, publicId, systemId) {
			const dt = makeDoctype(null, String(name));
			return dt;
		}
		createHTMLDocument(title) {
			const doc = mint(Document);
			doc._contentType = "text/html";
			insert(makeDoctype(doc, "html"), doc, null);
			const html = makeElement(doc, "html", HTML_NS);
			insert(html, doc, null);
			const head = makeElement(doc, "head", HTML_NS);
			insert(head, html, null);
			if (title !== undefined) {
				const t = makeElement(doc, "title", HTML_NS);
				insert(makeText(doc, String(title)), t, null);
				insert(t, head, null);
			}
			insert(makeElement(doc, "body", HTML_NS), html, null);
			return doc;
		}
		createDocument(ns, qname, doctype = null) {
			const doc = mint(Document);
			doc._contentType = ns === HTML_NS ? "application/xhtml+xml" : ns === SVG_NS ? "image/svg+xml" : "application/xml";
			if (doctype) insert(doctype, doc, null);
			if (qname) insert(makeElement(doc, String(qname), ns === "" ? null : ns), doc, null);
			return doc;
		}
	}

	class Document extends Node {
		constructor() {
			const wasMinting = minting;
			minting = true;
			super();
			minting = wasMinting;
			this._owner = null;
			this._contentType = "application/xml";
			this._url = null;
		}
		get nodeType() { return 9; }
		get nodeName() { return "#document"; }
		get ownerDocument() { return null; }
		get URL() {
			if (this._isMain && globalThis.location) return globalThis.location.href;
			return this._url ?? "about:blank";
		}
		get documentURI() { return this.URL; }
		get location() { return this._isMain ? (globalThis.location ?? null) : null; }
		get defaultView() { return this._isMain ? globalThis : null; }
		get compatMode() {
			return this._kids.some((k) => k instanceof DocumentType) ? "CSS1Compat" : "BackCompat";
		}
		get characterSet() { return "UTF-8"; }
		get charset() { return "UTF-8"; }
		get inputEncoding() { return "UTF-8"; }
		get contentType() { return this._contentType; }
		get readyState() { return this._readyState ?? "complete"; }
		get doctype() { return this._kids.find((k) => k instanceof DocumentType) ?? null; }
		get documentElement() { return this._kids.find((k) => k instanceof Element) ?? null; }
		get head() {
			const root = this.documentElement;
			return root?._kids.find((k) => k instanceof Element && k._local === "head" && k._ns === HTML_NS) ?? null;
		}
		get body() {
			const root = this.documentElement;
			return root?._kids.find((k) => k instanceof Element &&
				(k._local === "body" || k._local === "frameset") && k._ns === HTML_NS) ?? null;
		}
		set body(el) {
			if (!(el instanceof HTMLElement) || (el._local !== "body" && el._local !== "frameset")) {
				throw domError("the body must be a body or frameset element", "HierarchyRequestError");
			}
			const old = this.body;
			if (old) this.documentElement.replaceChild(el, old);
			else if (this.documentElement) this.documentElement.appendChild(el);
			else throw domError("there is no document element", "HierarchyRequestError");
		}
		get title() {
			const t = this.head?._kids.find((k) => k instanceof Element && k._local === "title");
			// Child text content, whitespace-collapsed, per the spec's getter.
			return t ? t._kids.filter((k) => k.nodeType === 3).map((k) => k._data).join("")
				.replace(/[ \t\n\f\r]+/g, " ").replace(/^ | $/g, "") : "";
		}
		set title(v) {
			let t = this.head?._kids.find((k) => k instanceof Element && k._local === "title");
			if (!t && this.head) {
				t = makeElement(this, "title", HTML_NS);
				insert(t, this.head, null);
			}
			if (t) t.textContent = String(v);
		}
		get implementation() {
			if (!this._impl) this._impl = mint(DOMImplementation);
			return this._impl;
		}
		get referrer() { return ""; }
		get scripts() { return byTagName(this, "script"); }
		get images() { return byTagName(this, "img"); }
		get links() {
			return makeHTMLCollection(() => [...elementDescendants(this)]
				.filter((el) => (el._local === "a" || el._local === "area") && el.hasAttribute("href")));
		}
		get forms() { return byTagName(this, "form"); }
		createElement(name) {
			name = String(name);
			if (!/^[A-Za-z][^\t\n\f\r /><\0]*$/.test(name)) {
				throw domError(`"${name}" is not a valid element name`, "InvalidCharacterError");
			}
			return makeElement(this, this._contentType === "text/html" || this._contentType === "application/xhtml+xml"
				? name.toLowerCase() : name, HTML_NS);
		}
		createElementNS(ns, qname) {
			ns = ns === "" || ns === undefined || ns === null ? null : String(ns);
			qname = String(qname);
			let prefix = null, local = qname;
			const colon = qname.indexOf(":");
			if (colon >= 0) { prefix = qname.slice(0, colon); local = qname.slice(colon + 1); }
			if (prefix !== null && ns === null) {
				throw domError("a prefixed name needs a namespace", "NamespaceError");
			}
			return makeElement(this, local, ns, prefix);
		}
		createTextNode(data) { return makeText(this, String(data)); }
		createComment(data) { return makeComment(this, String(data)); }
		createProcessingInstruction(target, data) { return makePI(this, String(target), String(data)); }
		createDocumentFragment() {
			const f = mint(DocumentFragment);
			f._owner = this;
			return f;
		}
		createAttribute(name) {
			const a = mint(Attr);
			a._local = String(name).toLowerCase();
			a._owner = this;
			return a;
		}
		createAttributeNS(ns, qname) {
			const a = mint(Attr);
			ns = ns === "" ? null : ns;
			qname = String(qname);
			const colon = qname.indexOf(":");
			if (colon >= 0) { a._prefix = qname.slice(0, colon); a._local = qname.slice(colon + 1); } else a._local = qname;
			a._ns = ns;
			a._owner = this;
			return a;
		}
		createEvent(kind) {
			// The legacy factory: an uninitialized event of the named family.
			const k = String(kind).toLowerCase();
			if (k === "event" || k === "events" || k === "htmlevents" || k === "svgevents") return new Event("");
			if (k === "customevent") return new CustomEvent("");
			if (k === "messageevent") return new MessageEvent("");
			throw domError(`"${kind}" is not an event interface`, "NotSupportedError");
		}
		getElementById(id) {
			id = String(id);
			if (id === "") return null;
			for (const el of elementDescendants(this)) if (el.id === id) return el;
			return null;
		}
		getElementsByTagName(name) { return byTagName(this, name); }
		getElementsByTagNameNS(ns, name) { return byTagNameNS(this, ns, name); }
		getElementsByClassName(names) { return byClassName(this, names); }
		getElementsByName(name) {
			name = String(name);
			return makeNodeList(() => [...elementDescendants(this)].filter((el) => el.getAttribute("name") === name));
		}
		importNode(node, deep = false) {
			if (!(node instanceof Node)) throw new TypeError("importNode: a Node is required");
			if (node instanceof Document) throw domError("a document cannot be imported", "NotSupportedError");
			const copy = cloneNode(node, Boolean(deep));
			adopt(copy, this);
			return copy;
		}
		adoptNode(node) {
			if (!(node instanceof Node)) throw new TypeError("adoptNode: a Node is required");
			if (node instanceof Document) throw domError("a document cannot be adopted", "NotSupportedError");
			remove(node);
			adopt(node, this);
			return node;
		}
	}
	Object.assign(Document.prototype, ParentNode);
	Object.defineProperty(Document.prototype, "children", {
		get() {
			if (!this._childrenView) this._childrenView = makeHTMLCollection(() => this._kids.filter((k) => k instanceof Element));
			return this._childrenView;
		},
		enumerable: true, configurable: true,
	});
	for (const g of ["firstElementChild", "lastElementChild", "childElementCount"]) {
		Object.defineProperty(Document.prototype, g,
			Object.getOwnPropertyDescriptor(Element.prototype, g));
	}

	// --------------------------------------------------------- DOMParser
	class DOMParser {
		parseFromString(text, type) {
			if (arguments.length < 2) throw new TypeError("parseFromString: text and type are required");
			type = String(type);
			if (type === "text/html") {
				const doc = mint(Document);
				doc._contentType = "text/html";
				buildParsedInto(doc, String(text), "");
				return doc;
			}
			// The XML side needs an XML parser; refusing is honest, quietly
			// returning an empty document is not. TODO: encoding/xml-backed
			// parsing for the four XML types.
			throw new TypeError(`parseFromString: ${type} is not supported yet`);
		}
	}

	// ------------------------------------------------- parse + serialize
	function buildParsedInto(parent, text, contextTag) {
		const doc = parent instanceof Document ? parent : parent._owner ?? mainDocument;
		const raw = ops.dom_parse_html(text, contextTag ?? (parent instanceof Element ? parent._local : "body"));
		const tree = JSON.parse(String(raw));
		if (tree && tree.error) throw domError(String(tree.error), "SyntaxError");
		const build = (enc, into) => {
			switch (enc[0]) {
				case 1: {
					const [, name, pns, attrs, kids] = enc;
					const el = makeElement(doc, name, NS_BY_PARSER[pns] ?? pns);
					for (let i = 0; i < attrs.length; i += 3) {
						setAttrRaw(el, attrs[i], attrs[i + 1] === "" ? null : attrs[i + 1], null, attrs[i + 2]);
					}
					insert(el, into, null);
					for (const k of kids) build(k, el);
					break;
				}
				case 3: insert(makeText(doc, enc[1]), into, null); break;
				case 8: insert(makeComment(doc, enc[1]), into, null); break;
				case 10: if (into instanceof Document) insert(makeDoctype(doc, enc[1]), into, null); break;
			}
		};
		for (const enc of tree) build(enc, parent);
	}

	const VOID = new Set(["area", "base", "br", "col", "embed", "hr", "img", "input",
		"link", "meta", "source", "track", "wbr"]);
	const RAW_TEXT = new Set(["style", "script", "xmp", "iframe", "noembed", "noframes", "plaintext"]);
	// The five-entry escape table of the HTML fragment serialization algorithm.
	const escapeText = (s) => s.replace(/&/g, "&amp;").replace(/ /g, "&nbsp;")
		.replace(/</g, "&lt;").replace(/>/g, "&gt;");
	const escapeAttr = (s) => s.replace(/&/g, "&amp;").replace(/ /g, "&nbsp;")
		.replace(/"/g, "&quot;");
	function serializeNode(n) {
		switch (n.nodeType) {
			case 1: {
				const name = n._ns === HTML_NS || n._ns === SVG_NS || n._ns === MATHML_NS
					? n._local : (n._prefix ? n._prefix + ":" + n._local : n._local);
				let out = "<" + name;
				for (const a of n._attrs) out += " " + a.name + '="' + escapeAttr(a._value) + '"';
				out += ">";
				if (n._ns === HTML_NS && VOID.has(n._local)) return out;
				out += serializeChildren(n);
				return out + "</" + name + ">";
			}
			case 3: {
				const p = n._parent;
				if (p instanceof Element && p._ns === HTML_NS && RAW_TEXT.has(p._local)) return n._data;
				return escapeText(n._data);
			}
			case 8: return "<!--" + n._data + "-->";
			case 7: return "<?" + n._piTarget + " " + n._data + ">";
			case 10: return "<!DOCTYPE " + n._name + ">";
			default: return serializeChildren(n);
		}
	}
	function serializeChildren(n) {
		let out = "";
		for (const k of n._kids) out += serializeNode(k);
		return out;
	}

	// -------------------------------------------------- selector matching
	const selectorCache = new Map();
	function compileSelector(text) {
		text = String(text);
		let ast = selectorCache.get(text);
		if (ast) return ast;
		ast = JSON.parse(String(ops.dom_parse_selector(text)));
		if (ast.error) throw domError(`"${text}" is not a valid selector: ${ast.error}`, "SyntaxError");
		if (selectorCache.size > 1000) selectorCache.clear();
		selectorCache.set(text, ast);
		return ast;
	}
	function matchesSelector(el, list, scope) {
		return list.some((complex) => matchComplex(el, complex, complex.length - 1, scope));
	}
	function matchComplex(el, steps, i, scope) {
		if (!matchCompound(el, steps[i].s, scope)) return false;
		if (i === 0) return true;
		const comb = steps[i].c;
		switch (comb) {
			case ">":
				return el._parent instanceof Element && matchComplex(el._parent, steps, i - 1, scope);
			case " ": {
				for (let p = el._parent; p instanceof Element; p = p._parent) {
					if (matchComplex(p, steps, i - 1, scope)) return true;
				}
				return false;
			}
			case "+": {
				const prev = el.previousElementSibling;
				return prev !== null && matchComplex(prev, steps, i - 1, scope);
			}
			case "~": {
				for (let p = el.previousElementSibling; p; p = p.previousElementSibling) {
					if (matchComplex(p, steps, i - 1, scope)) return true;
				}
				return false;
			}
			default:
				return false;
		}
	}
	function nthMatch(ab, idx) {
		const [a, b] = ab;
		if (a === 0) return idx === b;
		const n = (idx - b) / a;
		return Number.isInteger(n) && n >= 0;
	}
	function elementIndex(el, ofType, fromEnd) {
		const sibs = el._parent ? el._parent._kids.filter((k) => k instanceof Element &&
			(!ofType || (k._local === el._local && k._ns === el._ns))) : [el];
		const i = sibs.indexOf(el);
		return fromEnd ? sibs.length - i : i + 1;
	}
	// The constraint-validation state, from the attributes — all the state a
	// renderless runtime has. An input/textarea is invalid when required and
	// empty; a select when required and its chosen option's value is empty; a
	// fieldset (or form) when any submittable descendant is invalid.
	function elementInvalid(el) {
		if (el.hasAttribute("disabled")) return false;
		switch (el._local) {
			case "input":
				return el.hasAttribute("required") && !(el.getAttribute("value") ?? "");
			case "textarea":
				return el.hasAttribute("required") && el.textContent === "";
			case "select": {
				if (!el.hasAttribute("required")) return false;
				const options = [...elementDescendants(el)].filter((o) => o._local === "option");
				const chosen = options.find((o) => o.hasAttribute("selected")) ?? options[0];
				if (!chosen) return true;
				return (chosen.getAttribute("value") ?? chosen.textContent) === "";
			}
			case "fieldset": case "form": {
				for (const d of elementDescendants(el)) {
					if (["input", "textarea", "select"].includes(d._local) && elementInvalid(d)) return true;
				}
				return false;
			}
			default:
				return null; // not a candidate: matches neither :valid nor :invalid
		}
	}

	// :has(relative-list): does any element, taken relative to el, match? The
	// relative complex is evaluated as if prefixed by an anchor at el — which
	// is NOT the same thing as :scope: a ":has(> :scope)" argument anchors at
	// the candidate while its :scope still means the outer scoping root, so
	// the anchor is its own (internal) pseudo, carried on a stack because
	// :has can nest through :is/:not.
	const anchorStack = [];
	function hasMatch(el, list, scope) {
		anchorStack.push(el);
		try {
			for (const complex of list) {
				const anchored = [{ c: "", s: { p: [{ n: "__anchor" }] } },
					{ c: complex[0].c || " ", s: complex[0].s }, ...complex.slice(1)];
				const root = el.getRootNode();
				const space = root instanceof Element || root instanceof Document || root instanceof DocumentFragment
					? elementDescendants(root) : elementDescendants(el);
				for (const x of space) {
					if (matchComplex(x, anchored, anchored.length - 1, scope)) return true;
				}
			}
			return false;
		} finally {
			anchorStack.pop();
		}
	}

	function matchCompound(el, c, scope) {
		if (c.t && c.t !== "*") {
			if (el._ns === HTML_NS ? el._local !== c.t : el._local.toLowerCase() !== c.t) return false;
		}
		if (c.i && el.id !== c.i) return false;
		if (c.cl) {
			const have = splitTokens(el.getAttribute("class"));
			for (const cl of c.cl) if (!have.includes(cl)) return false;
		}
		if (c.a) {
			for (const a of c.a) {
				let v = el.getAttribute(a.n);
				if (v === null) return false;
				if (!a.op) continue;
				let want = a.v;
				if (a.ci) { v = v.toLowerCase(); want = want.toLowerCase(); }
				switch (a.op) {
					case "=": if (v !== want) return false; break;
					case "~=": if (!splitTokens(v).includes(want) || want === "") return false; break;
					case "|=": if (v !== want && !v.startsWith(want + "-")) return false; break;
					case "^=": if (want === "" || !v.startsWith(want)) return false; break;
					case "$=": if (want === "" || !v.endsWith(want)) return false; break;
					case "*=": if (want === "" || !v.includes(want)) return false; break;
				}
			}
		}
		if (c.p) {
			for (const ps of c.p) {
				switch (ps.n) {
					case "first-child": if (el.previousElementSibling) return false; break;
					case "last-child": if (el.nextElementSibling) return false; break;
					case "only-child": if (el.previousElementSibling || el.nextElementSibling) return false; break;
					case "first-of-type": if (elementIndex(el, true, false) !== 1) return false; break;
					case "last-of-type": if (elementIndex(el, true, true) !== 1) return false; break;
					case "only-of-type": if (elementIndex(el, true, false) !== 1 || elementIndex(el, true, true) !== 1) return false; break;
					case "nth-child": if (!nthMatch(ps.ab, elementIndex(el, false, false))) return false; break;
					case "nth-last-child": if (!nthMatch(ps.ab, elementIndex(el, false, true))) return false; break;
					case "nth-of-type": if (!nthMatch(ps.ab, elementIndex(el, true, false))) return false; break;
					case "nth-last-of-type": if (!nthMatch(ps.ab, elementIndex(el, true, true))) return false; break;
					case "root": if (el !== (el._owner ?? el.getRootNode()).documentElement) return false; break;
					case "empty": if (el._kids.length > 0) return false; break;
					case "scope": if (scope ? el !== scope : el !== (el._owner?.documentElement ?? null)) return false; break;
					case "not": if (matchesSelector(el, ps.l, scope)) return false; break;
					case "has": if (!hasMatch(el, ps.l, scope)) return false; break;
					case "__anchor": if (el !== anchorStack[anchorStack.length - 1]) return false; break;
					case "valid": if (elementInvalid(el) !== false) return false; break;
					case "invalid": if (elementInvalid(el) !== true) return false; break;
					case "required":
						if (!["input", "textarea", "select"].includes(el._local) || !el.hasAttribute("required")) return false;
						break;
					case "optional":
						if (!["input", "textarea", "select"].includes(el._local) || el.hasAttribute("required")) return false;
						break;
					case "is": case "where": if (!matchesSelector(el, ps.l, scope)) return false; break;
					// The form-state pseudo-classes read the attribute state,
					// which is all the state a renderless runtime has.
					case "disabled": if (!el.hasAttribute("disabled")) return false; break;
					case "enabled": {
						const formy = ["button", "input", "select", "textarea", "optgroup", "option", "fieldset"];
						if (!formy.includes(el._local) || el.hasAttribute("disabled")) return false;
						break;
					}
					case "checked": if (!el.hasAttribute("checked") && !el.hasAttribute("selected")) return false; break;
					case "link": case "any-link":
						if (!((el._local === "a" || el._local === "area") && el.hasAttribute("href"))) return false;
						break;
					case "visited": return false;
					// Parse-valid, never matched: there is no pointer, no
					// focus and no fragment navigation here.
					case "hover": case "active": case "focus":
					case "focus-within": case "focus-visible": case "target":
						return false;
					case "defined": break;
					default: return false;
				}
			}
		}
		return true;
	}

	// ------------------------------------------------- event propagation
	// dispatchEvent on a Node travels the tree: capture from the root down,
	// target, then bubble back up when the event bubbles. The listener store
	// is the EventTarget's own (_listeners); this dispatcher only adds a path.
	function invokeListeners(node, event, capturePhase) {
		const list = node._listeners && node._listeners.get(event.type);
		if (!list) return;
		event._currentTarget = node;
		for (const l of [...list]) {
			if (l.removed) continue;
			if (l.capture !== capturePhase) continue;
			if (l.once) node.removeEventListener(event.type, l.callback, { capture: l.capture });
			try {
				if (typeof l.callback === "function") l.callback.call(node, event);
				else if (l.callback && typeof l.callback.handleEvent === "function") l.callback.handleEvent(event);
			} catch (err) {
				if (typeof globalThis.reportError === "function") globalThis.reportError(err);
			}
			if (event._stopImmediate) return;
		}
	}
	Node.prototype.dispatchEvent = function dispatchEvent(event, opts) {
		if (arguments.length < 1) throw new TypeError("dispatchEvent requires 1 argument");
		if (!(event instanceof Event)) throw new TypeError("dispatchEvent: the argument must be an Event");
		if (!(opts && opts.__keepTrusted)) event._trusted = false;
		event._target = this;
		event._stopped = false;
		event._stopImmediate = false;
		const ancestors = [];
		for (let n = this._parent; n; n = n._parent) ancestors.push(n);
		try {
			event._phase = 1; // CAPTURING_PHASE
			for (let i = ancestors.length - 1; i >= 0 && !event._stopped; i--) {
				invokeListeners(ancestors[i], event, true);
			}
			if (!event._stopped) {
				event._phase = 2; // AT_TARGET
				invokeListeners(this, event, true);
				if (!event._stopImmediate && !event._stopped) invokeListeners(this, event, false);
			}
			if (event.bubbles) {
				event._phase = 3; // BUBBLING_PHASE
				for (const a of ancestors) {
					if (event._stopped) break;
					invokeListeners(a, event, false);
				}
			}
		} finally {
			event._phase = 0;
			event._currentTarget = null;
			event._phase = null;
		}
		return !event.defaultPrevented;
	};

	// The legacy initEvent, which createEvent's callers pair with.
	if (!("initEvent" in Event.prototype)) {
		Object.defineProperty(Event.prototype, "initEvent", {
			value: function initEvent(type, bubbles = false, cancelable = false) {
				if (arguments.length < 1) throw new TypeError("initEvent: a type is required");
				this._type = String(type);
				this._bubbles = Boolean(bubbles);
				this._cancelable = Boolean(cancelable);
			},
			writable: true, enumerable: true, configurable: true,
		});
	}

	// ------------------------------------------------------------ install
	for (const [name, cls] of Object.entries({
		Node, CharacterData, Text, Comment, ProcessingInstruction, DocumentType,
		Attr, NamedNodeMap, DOMTokenList, Element, HTMLElement, HTMLUnknownElement,
		HTMLHtmlElement, HTMLHeadElement, HTMLBodyElement, HTMLDivElement,
		HTMLSpanElement, HTMLTitleElement, HTMLScriptElement, HTMLAnchorElement,
		NodeList, HTMLCollection, DocumentFragment, DOMImplementation, Document,
		DOMParser,
	})) {
		Object.defineProperty(cls.prototype, Symbol.toStringTag, { value: name, configurable: true });
		Object.defineProperty(globalThis, name, {
			value: cls, writable: true, enumerable: false, configurable: true,
		});
	}

	// The environment's document: the about:blank shape, with a doctype so a
	// standards-mode assertion holds; its URL tracks location live.
	mainDocument = mint(Document);
	mainDocument._contentType = "text/html";
	mainDocument._isMain = true;
	mainDocument._readyState = "complete";
	insert(makeDoctype(mainDocument, "html"), mainDocument, null);
	{
		const html = makeElement(mainDocument, "html", HTML_NS);
		insert(html, mainDocument, null);
		insert(makeElement(mainDocument, "head", HTML_NS), html, null);
		insert(makeElement(mainDocument, "body", HTML_NS), html, null);
	}
	Object.defineProperty(globalThis, "document", {
		value: mainDocument, writable: false, enumerable: true, configurable: true,
	});
})();
