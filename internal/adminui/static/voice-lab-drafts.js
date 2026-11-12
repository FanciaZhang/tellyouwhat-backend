// Local input belongs to its author. Only an explicit sample deletion removes it.
const voiceDrafts = (() => {
  let connection;
  const writer = sessionStorage.getItem('voice-lab-tab') || crypto.randomUUID();
  sessionStorage.setItem('voice-lab-tab', writer);
  const revisions = new Map();
  async function database() {
    if (!connection) connection = new Promise((resolve, reject) => {
      const request = indexedDB.open('tellyouwhat-voice-lab', 1);
      request.onupgradeneeded = () => {
        const db = request.result;
        db.createObjectStore('samples', {keyPath:['owner','id']}).createIndex('owner','owner');
        db.createObjectStore('parts', {keyPath:['owner','sample','recording','index']}).createIndex('recording',['owner','sample','recording']);
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error);
    });
    return connection;
  }
  async function transact(stores, mode, work) {
    const db = await database();
    return new Promise((resolve, reject) => {
      const tx = db.transaction(stores, mode);let result;
      tx.oncomplete = () => resolve(result);
      tx.onabort = tx.onerror = () => reject(tx.error || Error('无法保存本机草稿，请导出录音并保留本页。'));
      work(tx, value => result=value);
    });
  }
  return {
    async space(bytes){if(navigator.storage?.estimate){const {quota,usage}=await navigator.storage.estimate();if(quota&&quota-usage<bytes)throw Error('本机存储空间不足，请导出或删除旧样本后重试。');}},
    async collect(owner){
      return transact(['samples','parts'],'readwrite',tx=>{
        const request=tx.objectStore('samples').index('owner').getAll(owner);
        request.onsuccess=()=>{const active=new Map(request.result.map(row=>[row.id,row.recording?.id]));const cursor=tx.objectStore('parts').openCursor();cursor.onsuccess=()=>{const part=cursor.result;if(part){if(part.value.owner===owner&&active.get(part.value.sample)!==part.value.recording)part.delete();part.continue();}};};
      });
    },
    async list(owner) {
      return transact(['samples'],'readonly',(tx,done)=>{
        const request=tx.objectStore('samples').index('owner').getAll(owner);
        request.onsuccess=()=>done(request.result.map(({id,title,updatedAt})=>({id,title,updatedAt})).sort((a,b)=>b.updatedAt-a.updatedAt));
      });
    },
    async get(owner,id) {
      return transact(['samples'],'readonly',(tx,done)=>{
        const request=tx.objectStore('samples').get([owner,id]);
        request.onsuccess=()=>{const row=request.result;if(row)revisions.set(owner+':'+id,row.revision);done(row);};
      });
    },
    async save(row) {
      const key=row.owner+':'+row.id;
      const revision=await transact(['samples'],'readwrite',(tx,done)=>{
        const store=tx.objectStore('samples'),request=store.get([row.owner,row.id]);
        request.onsuccess=()=>{
          const previous=request.result;
          if(previous && previous.revision !== revisions.get(key) && previous.writer !== writer){tx.abort();return;}
          const revision=(previous?.revision||0)+1;
          store.put({...row,writer,revision,updatedAt:Date.now()});done(revision);
        };
      });
      revisions.set(key,revision);
      return revision;
    },
    async part(owner,sample,recording,index,blob) {
      return transact(['parts'],'readwrite',tx=>tx.objectStore('parts').put({owner,sample,recording,index,blob}));
    },
    async parts(owner,sample,recording) {
      return transact(['parts'],'readonly',(tx,done)=>{
        const request=tx.objectStore('parts').index('recording').getAll([owner,sample,recording]);
        request.onsuccess=()=>done(request.result.sort((a,b)=>a.index-b.index).map(p=>p.blob));
      });
    },
    async deleteParts(owner,sample,recording) {
      return transact(['parts'],'readwrite',tx=>{
        const request=tx.objectStore('parts').index('recording').openKeyCursor(IDBKeyRange.only([owner,sample,recording]));
        request.onsuccess=()=>{const cursor=request.result;if(cursor){tx.objectStore('parts').delete(cursor.primaryKey);cursor.continue();}};
      });
    },
    async remove(owner,id) {
      return transact(['samples','parts'],'readwrite',tx=>{
        tx.objectStore('samples').delete([owner,id]);
        const request=tx.objectStore('parts').openCursor();
        request.onsuccess=()=>{const cursor=request.result;if(cursor){if(cursor.value.owner===owner&&cursor.value.sample===id)cursor.delete();cursor.continue();}};
      });
    }
  };
})();
