// Correlate an official checkout tax response with this order's saved billing.
// Addresses stay in the private browser process and never enter diagnostics.
const normalize=value=>typeof value==='string'?value.trim().replace(/\s+/g,' ').toUpperCase():''
export class HostedQuote {
  constructor(id,billing) {
    this.path='/v1/payment_pages/'+id
    this.address={country:'US',line1:billing?.line1,city:billing?.city,state:billing?.state,postal_code:billing?.postal_code}
    this.pending=new Map();this.revision=0;this.candidate=null;this.changedAt=0
  }
  start(req) {
    const u=new URL(req.url())
    if(req.method()!=='POST'||u.hostname!=='api.stripe.com'||u.pathname!==this.path)return
    const form=new URLSearchParams(req.postData()||'')
    const matches=Object.entries(this.address).every(([key,value])=>{
      const values=form.getAll('tax_region['+key+']')
      return normalize(value)!==''&&values.length===1&&normalize(values[0])===normalize(value)
    })
    this.pending.set(req,{revision:++this.revision,matches})
    this.candidate=null
  }
  observe(req,status,snapshot) {
    const update=this.pending.get(req)
    if(!update)return
    this.pending.delete(req)
    // A late response to an older/partial address is not proof of the final
    // address. Fail closed until the page produces another matching update.
    this.candidate=status===200&&update.revision===this.revision&&update.matches&&snapshot?.taxReady&&snapshot.open&&!snapshot.paid?snapshot:null
    this.changedAt=Date.now()
  }
  failed(req) {
    if(this.pending.delete(req))this.candidate=null
  }
  current(snapshot) {return !!this.candidate&&this.candidate===snapshot&&this.pending.size===0}
  ready(snapshot) {return this.current(snapshot)&&Date.now()-this.changedAt>1500}
}
