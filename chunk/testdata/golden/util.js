'use strict';

const fs = require('fs');
const path = require('path');

/**
 * Reads a JSON file, or returns fallback when it is missing.
 */
function readJSON(file, fallback) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch (err) {
    if (err.code === 'ENOENT') {
      return fallback;
    }
    throw err;
  }
}

class Cache {
  constructor(dir) {
    this.dir = dir;
    this.mem = new Map();
  }

  key(name) {
    return path.join(this.dir, name.replace(/[^a-z0-9]/gi, '_') + '.json');
  }

  get(name) {
    if (this.mem.has(name)) {
      return this.mem.get(name);
    }
    const value = readJSON(this.key(name), undefined);
    this.mem.set(name, value);
    return value;
  }

  set(name, value) {
    this.mem.set(name, value);
    fs.writeFileSync(this.key(name), JSON.stringify(value));
  }
}

const debounce = (fn, ms) => {
  let t;
  return (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
};

module.exports = { readJSON, Cache, debounce };
