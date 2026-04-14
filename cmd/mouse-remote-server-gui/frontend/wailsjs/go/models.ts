export namespace main {
	
	export class ConfigDTO {
	    addr: string;
	    token: string;
	    relayAddr: string;
	    session: string;
	
	    static createFrom(source: any = {}) {
	        return new ConfigDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.addr = source["addr"];
	        this.token = source["token"];
	        this.relayAddr = source["relayAddr"];
	        this.session = source["session"];
	    }
	}

}

